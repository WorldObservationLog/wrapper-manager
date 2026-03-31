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
	"google.golang.org/grpc/keepalive"
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
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.Errorf("stream recv error: %v", err)
			return err
		}

		if req.Data.AdamId == "KEEPALIVE" {
			// KEEPALIVE 必须能够快速响应，不能因为此时正在发送大块解密数据（网络拥塞）而被 sendMu 长时间阻塞。
			// 如果获取不到锁，说明底层 TCP 缓冲区可能满载正在发送数据，此时不仅没空发心跳，并且“正在发数据”本身就起到了保活作用，所以就算跳过此次心跳也没有关系。
			if sendMu.TryLock() {
				err = stream.Send(&pb.DecryptReply{
					Header: &pb.ReplyHeader{Code: 0, Msg: "SUCCESS"},
					Data:   &pb.DecryptData{AdamId: "KEEPALIVE"},
				})
				sendMu.Unlock()
				if err != nil {
					log.Errorf("failed to send KEEPALIVE reply: %v", err)
					return err
				}
			} else {
				log.Debug("Skipped sending KEEPALIVE reply because stream is busy sending data")
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

			select {
			case <-ctx.Done():
				// 如果流已被客户端关闭或发生错误中断，直接丢弃结果，防止向 closed stream 写入引发 panic
				return
			case result := <-task.Result:
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
				// 再次检查 context 状态，避免在获取锁的等待期间流已经被关闭
				if ctx.Err() == nil {
					if err := stream.Send(reply); err != nil {
						log.Errorf("failed to send decrypt reply to %s: %v", task.AdamId, err)
						cancel() // 通知其他 goroutine 停止发送
					}
				}
				sendMu.Unlock()
			}
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
	instance, err := GlobalManager.SelectM3U8Instance(req.Data.AdamId)
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
	instance, err := GlobalManager.SelectWebInstance(req.Data.AdamId)
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
	instance, err := GlobalManager.SelectWebInstance(req.Data.AdamId)
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

	go func() {
		watcherTicker := time.NewTicker(10 * time.Second)
		defer watcherTicker.Stop()
		for range watcherTicker.C {
			if GlobalManager == nil || !Ready {
				continue
			}
			list := GlobalManager.List()
			for _, inst := range list {
				inst.RecoverM3U8Health(5)

				inst.Lock()
				client := inst.Client
				m3Health := inst.M3U8Health
				inst.Unlock()

				isClientBroken := client != nil && client.IsBroken()
				isM3U8Unresponsive := m3Health <= 0

				if isClientBroken || isM3U8Unresponsive {
					reason := ""
					if isClientBroken {
						reason = "DecryptClient is broken"
					} else {
						reason = "M3U8 is unresponsive"
					}
					log.Warnf("Watchdog: Instance %s is dead (%s). Killing wrapper to trigger auto-restart.", inst.Id, reason)

					// Force kill to unblock cmd.Wait() in wrapperStart()
					err := KillWrapper(inst.Id)
					if err != nil {
						log.Errorf("Watchdog: Failed to kill instance %s: %v", inst.Id, err)
					}
				}
			}

			// Clean up memory leaks for old M3U8 failed records
			GlobalManager.CleanupFailedRecords(15 * time.Minute)
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
	opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
		Time:    30 * time.Second, // 每隔 30 秒向空闲的客户端发一次 Ping
		Timeout: 10 * time.Second, // 客户端如果 10 秒内不回 Ack 就断开
	}))
	opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             5 * time.Minute, // 客户端如果自行发送 ping, 频率至少要隔 5 分钟
		PermitWithoutStream: true,            // 允许客户端在没有任何 active stream（完全挂机）的时候给服务端发 Ping 续命
	}))
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
