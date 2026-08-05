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
	"runtime/debug"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

var (
	PROXY                string
	DeviceInfo           string
	Ready                atomic.Bool
	ShouldStartInstances int
	decryptBytes         atomic.Uint64
	decryptCount         atomic.Uint64
)

// maxConcurrentDecryptsPerStream 限制单条 Decrypt stream 在途解密 goroutine 上限，
// 满载时 Recv 循环阻塞形成背压，防止高速客户端打爆 goroutine / 内存。
const maxConcurrentDecryptsPerStream = 256

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
			Ready:       Ready.Load(),
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
			// 该账号已登录，不应再触发 WrapperInitial / 2FA 流程。
			continue
		}
		if req.Data.TwoStepCode != "" {
			if err := provide2FACode(id, req.Data.TwoStepCode); err != nil {
				log.Errorf("failed to provide 2fa code for %s: %v", id, err)
				if err := stream.Send(&pb.LoginReply{
					Header: &pb.ReplyHeader{Code: -1, Msg: "failed to submit 2fa code"},
				}); err != nil {
					return err
				}
			}
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
	err := KillWrapper(instance)
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

	// 有界并发：限制单条 stream 在途解密 goroutine 数量。
	// 信号量满时，Recv 循环自然阻塞，对客户端形成背压，避免无上限堆 goroutine / 内存。
	sem := make(chan struct{}, maxConcurrentDecryptsPerStream)

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

		// 获取信号量配额；流已关闭则立即退出，不再受理新 sample。
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sem <- struct{}{}:
		}

		// 将整个解密等待环节异步抛出，解放 gRPC 主 Recv() 循环的超高并发。
		go func(task Task) {
			defer func() { <-sem }()
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
	m3u8, err := GetM3U8(c, instance, req.Data.AdamId)
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

// recoveryUnaryInterceptor 捕获一元 handler 中的 panic，转为 Internal 错误返回，防止进程崩溃。
func recoveryUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("panic recovered in %s: %v\n%s", info.FullMethod, r, debug.Stack())
			err = status.Errorf(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

// recoveryStreamInterceptor 捕获流式 handler 中的 panic。
func recoveryStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("panic recovered in stream %s: %v\n%s", info.FullMethod, r, debug.Stack())
			err = status.Errorf(codes.Internal, "internal error")
		}
	}()
	return handler(srv, ss)
}

func main() {
	var host = flag.String("host", "localhost", "host of gRPC server")
	var port = flag.Int("port", 8080, "port of gRPC server")
	var mirror = flag.Bool("mirror", false, "use mirror to download wrapper and file (for Chinese users)")
	var debug_ = flag.Bool("debug", false, "enable debug output")
	var prepare = flag.Bool("prepare", false, "only download required files")
	var testInstances = flag.Bool("test-instances", false, "test whether saved instances can start, print a report, then exit")
	var testSource = flag.String("test-source", "json", "test mode instance source: \"json\" (instances.json) or \"dir\" (data/wrapper/rootfs/data/instances)")
	var testApply = flag.Bool("test-apply", false, "after testing, rewrite instances.json to keep only instances that started successfully")
	var testTimeout = flag.Int("test-timeout", 120, "per-instance startup timeout in seconds for test mode")
	var testConcurrency = flag.Int("test-concurrency", 4, "how many instances to test in parallel")
	flag.StringVar(&PROXY, "proxy", "", "proxy for wrapper and manager")
	flag.StringVar(&DeviceInfo, "device-info", "Music/5.0.2/Android/10/Pixel 10/7663314/en-US/en-US/dc28071e371c439e", "device info for wrapper")
	flag.Parse()

	log.SetOutput(os.Stdout)
	if *debug_ {
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

	if *testInstances {
		ok := RunInstanceTest(testConfig{
			source:      *testSource,
			apply:       *testApply,
			timeout:     time.Duration(*testTimeout) * time.Second,
			concurrency: *testConcurrency,
		})
		if ok {
			os.Exit(0)
		}
		os.Exit(1)
	}

	go func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Errorf("panic in speed monitor: %v\n%s", r, debug.Stack())
					}
				}()
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
			time.Sleep(time.Second)
		}
	}()

	go func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Errorf("panic in watchdog: %v\n%s", r, debug.Stack())
					}
				}()
				watcherTicker := time.NewTicker(10 * time.Second)
				defer watcherTicker.Stop()
				for range watcherTicker.C {
					if GlobalManager == nil {
						continue
					}
					list := GlobalManager.List()
					for _, inst := range list {
						if !inst.IsReady() {
							continue
						}

						m3HealthBefore := inst.GetM3U8Health()
						inst.RecoverM3U8Health(5)

						client := inst.GetClient()

						isClientBroken := client != nil && client.IsBroken()
						isM3U8Unresponsive := m3HealthBefore <= 0

						if isClientBroken || isM3U8Unresponsive {
							reason := ""
							if isClientBroken {
								reason = "DecryptClient is broken"
							} else {
								reason = "M3U8 is unresponsive"
							}
							log.Warnf("Watchdog: Instance %s is dead (%s). Killing wrapper to trigger auto-restart.", inst.Id, reason)

							err := KillWrapper(inst)
							if err != nil {
								log.Errorf("Watchdog: Failed to kill instance %s: %v", inst.Id, err)
							}
						}
					}

					GlobalManager.CleanupFailedRecords(15 * time.Minute)

					// 全局 Ready 反映实例层真实状态：只要至少一个实例就绪即为可用。
					// 旧逻辑只在 wrapperReady 置 true 且永不置回，会让客户端在全部实例
					// 不可用时仍看到 Ready=true 而继续发包。
					anyReady := false
					for _, inst := range GlobalManager.List() {
						if inst.IsReady() {
							anyReady = true
							break
						}
					}
					Ready.Store(anyReady)
				}
			}()
			time.Sleep(time.Second)
		}
	}()

	WMDispatcher = NewDispatcher()

	if _, err := os.Stat("data/instances.json"); !errors.Is(err, os.ErrNotExist) {
		GlobalManager = LoadInstance()
		list := GlobalManager.List()
		ShouldStartInstances = len(list)
		for _, inst := range list {
			go WrapperStart(inst.Id)
		}
	} else {
		GlobalManager = NewInstanceManager()
		ShouldStartInstances = 0
		Ready.Store(true)
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
	// panic-recovery 拦截器：handler 内的意外 panic（如 Apple 响应结构变化引发的断言失败）
	// 转为 gRPC 错误返回，避免拖垮整个进程。
	opts = append(opts, grpc.UnaryInterceptor(recoveryUnaryInterceptor))
	opts = append(opts, grpc.StreamInterceptor(recoveryStreamInterceptor))
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
