package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"os/user"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	pb "github.com/WorldObservationLog/wrapper-manager/proto"
	"github.com/gofrs/uuid/v5"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"
)

var (
	PROXY                string
	DeviceInfo           string
	Ready                bool
	ShouldStartInstances int
	decryptBytes         atomic.Uint64
	decryptCount         atomic.Uint64
)

type server struct {
	pb.UnimplementedWrapperManagerServiceServer
}

func (s *server) Status(c context.Context, req *emptypb.Empty) (*pb.StatusReply, error) {
	p, ok := peer.FromContext(c)
	if ok {
		log.Infof("status request from %s", p.Addr.String())
	} else {
		log.Infof("status request from unknown peer")
	}
	var regions []string
	list := GlobalManager.List()
	for _, instance := range list {
		if !slices.Contains(regions, instance.Region) {
			regions = append(regions, instance.Region)
		}
	}
	listCount := len(list)
	return &pb.StatusReply{
		Header: &pb.ReplyHeader{
			Code: 0,
			Msg:  "SUCCESS",
		},
		Data: &pb.StatusData{
			Status:      listCount != 0,
			Regions:     regions,
			ClientCount: int32(listCount),
			Ready:       Ready,
		},
	}, nil
}

func (s *server) Login(stream grpc.BidiStreamingServer[pb.LoginRequest, pb.LoginReply]) error {
	p, ok := peer.FromContext(stream.Context())
	if ok {
		log.Infof("login stream from %s", p.Addr.String())
	} else {
		log.Infof("login stream from unknown peer")
	}
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		id := uuid.NewV5(uuid.FromStringOrNil("77777777-7777-7777-7777-77777777"), req.Data.Username).String()
		instance := GlobalManager.Get(id)
		if instance != nil {
			err = stream.Send(&pb.LoginReply{
				Header: &pb.ReplyHeader{
					Code: -1,
					Msg:  "already login",
				},
			})
			if err != nil {
				return err
			}
		}
		if req.Data.TwoStepCode != "" {
			provide2FACode(id, req.Data.TwoStepCode)
		} else {
			LoginConnMap.Store(id, stream)
			go WrapperInitial(req.Data.Username, req.Data.Password)
		}
	}
}

func (s *server) Logout(c context.Context, req *pb.LogoutRequest) (*pb.LogoutReply, error) {
	p, ok := peer.FromContext(c)
	if ok {
		log.Infof("logout request from %s", p.Addr.String())
	} else {
		log.Infof("logout request from unknown peer")
	}
	id := uuid.NewV5(uuid.FromStringOrNil("77777777-7777-7777-7777-77777777"), req.Data.Username).String()
	instance := GlobalManager.Get(id)
	if instance == nil {
		return &pb.LogoutReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  "no such account",
			},
			Data: &pb.LogoutData{Username: req.Data.Username},
		}, nil
	}
	instance.NoRestart = true
	err := KillWrapper(instance.Id)
	if err != nil {
		return &pb.LogoutReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  "failed to kill wrapper",
			},
			Data: &pb.LogoutData{Username: req.Data.Username},
		}, nil
	}
	RemoveWrapperData(instance.Id)
	return &pb.LogoutReply{
		Header: &pb.ReplyHeader{
			Code: 0,
			Msg:  "SUCCESS",
		},
		Data: &pb.LogoutData{Username: req.Data.Username},
	}, nil
}

func (s *server) Decrypt(stream grpc.BidiStreamingServer[pb.DecryptRequest, pb.DecryptReply]) error {
	p, ok := peer.FromContext(stream.Context())
	if ok {
		log.Infof("decrypt stream from %s", p.Addr.String())
	} else {
		log.Infof("decrypt stream from unknown peer")
	}

	// 并发写保护：在一个 gRPC stream 内允许多个 goroutine 归还结果时，必须锁定以防止帧交错损坏。
	var sendMu sync.Mutex

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		if req.Data.AdamId == "KEEPALIVE" {
			err = stream.Send(&pb.DecryptReply{
				Header: &pb.ReplyHeader{Code: 0, Msg: "SUCCESS"},
				Data:   &pb.DecryptData{AdamId: "KEEPALIVE"},
			})
			if err != nil {
				return err
			}
			continue
		}

		// 致命问题隔离：避免底层 gRPC 流的数组发生内存重叠
		safePayload := make([]byte, len(req.Data.Sample))
		copy(safePayload, req.Data.Sample)

		task := Task{
			AdamId:      req.Data.AdamId,
			Key:         req.Data.Key,
			SampleIndex: req.Data.SampleIndex,
			Payload:     safePayload,
			Result:      make(chan *Result, 1),
		}

		// 将整个解密等待环节异步抛出，解放 gRPC 主 Recv() 循环的超高并发。
		go func(task Task) {
			WMDispatcher.Submit(&task)
			result := <-task.Result

			var reply *pb.DecryptReply
			if result.Error != nil {
				reply = &pb.DecryptReply{
					Header: &pb.ReplyHeader{Code: -1, Msg: result.Error.Error()},
					Data: &pb.DecryptData{
						AdamId:      task.AdamId,
						Key:         task.Key,
						Sample:      task.Payload,
						SampleIndex: task.SampleIndex,
					},
				}
			} else {
				decryptBytes.Add(uint64(len(task.Payload)))
				decryptCount.Add(1)
				reply = &pb.DecryptReply{
					Header: &pb.ReplyHeader{Code: 0, Msg: "SUCCESS"},
					Data: &pb.DecryptData{
						AdamId:      task.AdamId,
						Key:         task.Key,
						SampleIndex: task.SampleIndex,
						Sample:      result.Data,
					},
				}
			}

			// 写回给客户端的隧道受全局单锁保护
			sendMu.Lock()
			if err := stream.Send(reply); err != nil {
				log.Errorf("failed to send decrypt reply to %s: %v", task.AdamId, err)
			}
			sendMu.Unlock()
		}(task)
	}
}

func (s *server) M3U8(c context.Context, req *pb.M3U8Request) (*pb.M3U8Reply, error) {
	p, ok := peer.FromContext(c)
	if ok {
		log.Infof("m3u8 request from %s", p.Addr.String())
	} else {
		log.Infof("m3u8 request from unknown peer")
	}
	instance, err := GlobalManager.SelectInstance(req.Data.AdamId, "")
	if err != nil {
		return &pb.M3U8Reply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	if instance == nil {
		return &pb.M3U8Reply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  "no available instance",
			},
		}, nil
	}
	m3u8, err := GetM3U8(instance, req.Data.AdamId)
	if err != nil {
		GlobalManager.ReportFailure(req.Data.AdamId, instance.Id)
		return &pb.M3U8Reply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	if m3u8 == "" {
		GlobalManager.ReportFailure(req.Data.AdamId, instance.Id)
		return &pb.M3U8Reply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  fmt.Sprintf("failed to get m3u8 of adamId: %s", req.Data.AdamId),
			},
		}, nil
	}
	return &pb.M3U8Reply{
		Header: &pb.ReplyHeader{
			Code: 0,
			Msg:  "SUCCESS",
		},
		Data: &pb.M3U8DataResponse{
			AdamId: req.Data.AdamId,
			M3U8:   m3u8,
		},
	}, nil
}

func (s *server) Lyrics(c context.Context, req *pb.LyricsRequest) (*pb.LyricsReply, error) {
	p, ok := peer.FromContext(c)
	if ok {
		log.Infof("lyrics request from %s", p.Addr.String())
	} else {
		log.Infof("lyrics request from unknown peer")
	}

	var selectedInstance *WrapperInstance
	// Priority: Explicit region match in request
	list := GlobalManager.List()
	for _, instance := range list {
		if strings.ToUpper(instance.Region) == strings.ToUpper(req.Data.Region) {
			selectedInstance = instance
			break
		}
	}
	if selectedInstance == nil {
		selectedInstance = GlobalManager.SelectInstanceForLyrics(req.Data.AdamId, req.Data.Language)
		if selectedInstance == nil {
			return &pb.LyricsReply{
				Header: &pb.ReplyHeader{
					Code: -1,
					Msg:  "no available instance",
				},
			}, nil
		}
	}
	token, err := GetToken()
	if err != nil {
		return &pb.LyricsReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	musicToken, err := GetMusicToken(selectedInstance)
	if err != nil {
		return &pb.LyricsReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	inst := selectedInstance
	lyrics, err := GetLyrics(req.Data.AdamId, inst.Region, req.Data.Language, token, musicToken)
	if err != nil {
		return &pb.LyricsReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	return &pb.LyricsReply{
		Header: &pb.ReplyHeader{
			Code: 0,
			Msg:  "SUCCESS",
		},
		Data: &pb.LyricsDataResponse{
			AdamId: req.Data.AdamId,
			Lyrics: lyrics,
		},
	}, nil
}

func (s *server) WebPlayback(c context.Context, req *pb.WebPlaybackRequest) (*pb.WebPlaybackReply, error) {
	p, ok := peer.FromContext(c)
	if ok {
		log.Infof("webplayback request from %s", p.Addr.String())
	} else {
		log.Infof("webplayback request from unknown peer")
	}
	instance, err := GlobalManager.SelectInstance(req.Data.AdamId, "")
	if err != nil {
		return &pb.WebPlaybackReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	if instance == nil {
		return &pb.WebPlaybackReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  "no available instance",
			},
			Data: nil,
		}, nil
	}
	token, err := GetToken()
	if err != nil {
		return &pb.WebPlaybackReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	musicToken, err := GetMusicToken(instance)
	if err != nil {
		return &pb.WebPlaybackReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	m3u8, err := GetWebPlayback(req.Data.AdamId, token, musicToken)
	if err != nil {
		return &pb.WebPlaybackReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	return &pb.WebPlaybackReply{
		Header: &pb.ReplyHeader{
			Code: 0,
			Msg:  "SUCCESS",
		},
		Data: &pb.WebPlaybackDataResponse{
			AdamId: req.Data.AdamId,
			M3U8:   m3u8,
		},
	}, nil
}

func (s *server) License(c context.Context, req *pb.LicenseRequest) (*pb.LicenseReply, error) {
	p, ok := peer.FromContext(c)
	if ok {
		log.Infof("license request from %s", p.Addr.String())
	} else {
		log.Infof("license request from unknown peer")
	}
	instance, err := GlobalManager.SelectInstance(req.Data.AdamId, "")
	if err != nil {
		return &pb.LicenseReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	if instance == nil {
		return &pb.LicenseReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  "no available instance",
			},
			Data: nil,
		}, nil
	}
	token, err := GetToken()
	if err != nil {
		return &pb.LicenseReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	musicToken, err := GetMusicToken(instance)
	if err != nil {
		return &pb.LicenseReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	license, renew, err := GetLicense(req.Data.AdamId, req.Data.Challenge, req.Data.Uri, token, musicToken)
	if err != nil {
		return &pb.LicenseReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
			Data: nil,
		}, nil
	}
	return &pb.LicenseReply{
		Header: &pb.ReplyHeader{
			Code: 0,
			Msg:  "SUCCESS",
		},
		Data: &pb.LicenseDataResponse{
			AdamId:  req.Data.AdamId,
			License: license,
			Renew:   int64(renew),
		},
	}, nil
}

func newServer() *server {
	s := &server{}
	return s
}

func main() {
	var host = flag.String("host", "localhost", "host of gRPC server")
	var port = flag.Int("port", 8080, "port of gRPC server")
	var mirror = flag.Bool("mirror", false, "use mirror to download wrapper and file (for Chinese users)")
	var debug = flag.Bool("debug", false, "enable debug output")
	var prepare = flag.Bool("prepare", false, "only download required files")
	flag.StringVar(&PROXY, "proxy", "", "proxy for wrapper and manager")
	flag.StringVar(&DeviceInfo, "device-info", "Music/5.0.2/Android/10/Pixel 10/7663314/en-US/en-US/dc28071e371c439e", "device info for wrapper")
	flag.Parse()

	log.SetOutput(os.Stdout)
	if *debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	currentUser, err := user.Current()
	if err != nil {
		panic(err)
	}
	if currentUser.Uid != "0" {
		log.Panicln("root permission required")
	}

	if _, err := os.Stat("data/wrapper/wrapper"); errors.Is(err, os.ErrNotExist) {
		log.Warn("wrapper does not exist, downloading...")
		err = os.MkdirAll("data/wrapper", 0755)
		if err != nil {
			panic(err)
		}
		PrepareWrapper(*mirror)
	}

	if _, err := os.Stat("data/storefront_ids.json"); errors.Is(err, os.ErrNotExist) {
		log.Warn("storefront ids file dose not exist, downloading...")
		DownloadStorefrontIds()
	}

	if *prepare {
		os.Exit(0)
	}

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			bytesC := decryptBytes.Swap(0)
			countC := decryptCount.Swap(0)
			if countC > 0 {
				speedMB := float64(bytesC) / 1024.0 / 1024.0 / 5.0
				log.Infof("Decryption Speed: %.2f MB/s | %d samples processed in last 5s (%.1f req/s)", speedMB, countC, float64(countC)/5.0)
			}
		}
	}()

	WMDispatcher = NewDispatcher()

	if _, err := os.Stat("data/instances.json"); !errors.Is(err, os.ErrNotExist) {
		GlobalManager = LoadInstance()
		list := GlobalManager.List()
		ShouldStartInstances = len(list)
		for _, inst := range list {
			go WrapperStart(inst.Id, nil)
		}
	} else {
		GlobalManager = NewInstanceManager()
		ShouldStartInstances = 0
		Ready = true
	}

	log.Printf("wrapperManager running at %s:%d", *host, *port)
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	pb.RegisterWrapperManagerServiceServer(grpcServer, newServer())
	reflection.Register(grpcServer)

	go func() {
		log.Printf("wrapperManager running at %s:%d", *host, *port)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server with
	// a timeout of 5 seconds.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	GlobalManager.StopAll()
	grpcServer.GracefulStop()
	log.Println("Server exiting")
}
