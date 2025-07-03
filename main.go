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
	"google.golang.org/protobuf/types/known/emptypb"
	"io"
	"net"
	"os"
	"os/user"
	"slices"
	pb "wrapper-manager/proto"
)

var PROXY string

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
		if req.Data.TwoStepCode != 0 {
			provide2FACode(id, string(req.Data.TwoStepCode))
		} else {
			LoginConnMap.Store(id, stream)
			go WrapperInitial(req.Data.Username, req.Data.Password)
		}
	}
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
		SchedulerInstance.Submit(&task)
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
		if instance.Region == req.Data.Region {
			selectedInstanceId = instance.Id
		}
	}
	if selectedInstanceId == "" {
		selectedInstanceId = SelectInstance(req.Data.AdamId)
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
	dsid, accessToken, err := GetInstanceAuthToken(GetInstance(selectedInstanceId))
	if err != nil {
		return &pb.LyricsReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  err.Error(),
			},
		}, nil
	}
	lyrics, err := GetLyrics(req.Data.AdamId, req.Data.Region, req.Data.Language, dsid, token, accessToken)
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
		log.Infof("webplayback request from %s", p.Addr.String())
	} else {
		log.Infof("webplayback request from unknown peer")
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
	flag.StringVar(&PROXY, "proxy", "", "proxy for wrapper and manager")
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
	if currentUser.Name != "root" {
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

	SchedulerInstance = NewScheduler(3)

	Instances = make([]*WrapperInstance, 0)
	if _, err := os.Stat("data/storefront_ids.json"); !errors.Is(err, os.ErrNotExist) {
		for _, inst := range LoadInstance() {
			go WrapperStart(inst.Id)
		}
	}

	log.Printf("wrapperManager running at %s:%d", *host, *port)
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	pb.RegisterWrapperManagerServiceServer(grpcServer, newServer())
	grpcServer.Serve(lis)
}
