package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/gofrs/uuid/v5"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"net"
	"os"
	"os/user"
	"slices"
	"strings"
	pb "wrapper-manager/proto"
)

var (
	PROXY                string
	DeviceInfo           string
	Ready                bool
	ShouldStartInstances int
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
	for _, instance := range Instances {
		if !slices.Contains(regions, instance.Region) {
			regions = append(regions, instance.Region)
		}
	}
	return &pb.StatusReply{
		Header: &pb.ReplyHeader{
			Code: 0,
			Msg:  "SUCCESS",
		},
		Data: &pb.StatusData{
			Status:      len(Instances) != 0,
			Regions:     regions,
			ClientCount: int32(len(Instances)),
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
		for _, instance := range Instances {
			if instance.Id == id {
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
	instance := GetInstance(id)
	if instance.Id == "" {
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
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if req.Data.AdamId == "KEEPALIVE" {
			_ = stream.Send(&pb.DecryptReply{
				Header: &pb.ReplyHeader{
					Code: 0,
					Msg:  "SUCCESS",
				},
				Data: &pb.DecryptData{
					AdamId: "KEEPALIVE",
				},
			})
			continue
		}
		task := Task{
			AdamId:  req.Data.AdamId,
			Key:     req.Data.Key,
			Payload: req.Data.Sample,
			Result:  make(chan *Result),
		}
		available := false
		for _, inst := range Instances {
			if checkAvailableOnRegion(req.Data.AdamId, inst.Region, false) {
				available = true
				break
			}
		}
		if !available {
			_ = stream.Send(&pb.DecryptReply{
				Header: &pb.ReplyHeader{
					Code: -1,
					Msg:  "no available instance",
				},
				Data: &pb.DecryptData{
					AdamId:      req.Data.AdamId,
					Key:         req.Data.Key,
					Sample:      req.Data.Sample,
					SampleIndex: req.Data.SampleIndex,
				},
			})
			continue
		}
		go WMDispatcher.Submit(&task)
		result := <-task.Result
		if result.Error != nil {
			_ = stream.Send(&pb.DecryptReply{
				Header: &pb.ReplyHeader{
					Code: -1,
					Msg:  result.Error.Error(),
				},
				Data: &pb.DecryptData{
					AdamId:      req.Data.AdamId,
					Key:         req.Data.Key,
					Sample:      req.Data.Sample,
					SampleIndex: req.Data.SampleIndex,
				},
			})
		} else {
			_ = stream.Send(&pb.DecryptReply{
				Header: &pb.ReplyHeader{
					Code: 0,
					Msg:  "SUCCESS",
				},
				Data: &pb.DecryptData{
					AdamId:      req.Data.AdamId,
					Key:         req.Data.Key,
					SampleIndex: req.Data.SampleIndex,
					Sample:      result.Data,
				},
			})
		}
	}
}

func (s *server) M3U8(c context.Context, req *pb.M3U8Request) (*pb.M3U8Reply, error) {
	p, ok := peer.FromContext(c)
	if ok {
		log.Infof("m3u8 request from %s", p.Addr.String())
	} else {
		log.Infof("m3u8 request from unknown peer")
	}
	instanceID := SelectInstance(req.Data.AdamId)
	if instanceID == "" {
		return &pb.M3U8Reply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  "no available instance",
			},
		}, nil
	}
	m3u8, err := GetM3U8(GetInstance(instanceID), req.Data.AdamId)
	if err != nil {
		return &pb.M3U8Reply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	if m3u8 == "" {
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
	var selectedInstanceId string
	for _, instance := range Instances {
		if strings.ToUpper(instance.Region) == strings.ToUpper(req.Data.Region) {
			selectedInstanceId = instance.Id
		}
	}
	if selectedInstanceId == "" {
		selectedInstanceId = SelectInstanceForLyrics(req.Data.AdamId, req.Data.Language)
		if selectedInstanceId == "" {
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
	musicToken, err := GetMusicToken(GetInstance(selectedInstanceId))
	if err != nil {
		return &pb.LyricsReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	inst := GetInstance(selectedInstanceId)
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
	instanceID := SelectInstance(req.Data.AdamId)
	if instanceID == "" {
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
	musicToken, err := GetMusicToken(GetInstance(instanceID))
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
	instanceID := SelectInstance(req.Data.AdamId)
	if instanceID == "" {
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
	musicToken, err := GetMusicToken(GetInstance(instanceID))
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
		err = os.MkdirAll("data/wrapper", 0777)
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

	WMDispatcher = NewDispatcher()

	Instances = make([]*WrapperInstance, 0)
	if _, err := os.Stat("data/instances.json"); !errors.Is(err, os.ErrNotExist) {
		instancesInFile := LoadInstance()
		ShouldStartInstances = len(instancesInFile)
		for _, inst := range instancesInFile {
			go WrapperStart(inst.Id)
		}
	} else {
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
	grpcServer.Serve(lis)
}
