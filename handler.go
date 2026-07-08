package main

import (
	"log"
	"sync"

	pb "github.com/WorldObservationLog/wrapper-manager/proto"
	"google.golang.org/grpc"
)

var LoginConnMap = sync.Map{}

func Login2FAHandler(id string) {
	conn, ok := LoginConnMap.Load(id)
	if !ok || conn == nil {
		// 无对应登录流（时序异常/重启后重复触发）。不做断言，避免 nil 断言 panic 崩溃进程。
		log.Printf("Login2FAHandler: no pending login stream for %s", id)
		return
	}
	stream, ok := conn.(grpc.BidiStreamingServer[pb.LoginRequest, pb.LoginReply])
	if !ok {
		log.Printf("Login2FAHandler: unexpected conn type for %s", id)
		return
	}
	err := stream.Send(
		&pb.LoginReply{
			Header: &pb.ReplyHeader{
				Code: 2,
				Msg:  "2fa code require",
			},
		})
	if err != nil {
		log.Println(err)
	}
}

func LoginDoneHandler(id string) {
	GlobalManager.Save()
	conn, _ := LoginConnMap.LoadAndDelete(id)
	stream, ok := conn.(grpc.BidiStreamingServer[pb.LoginRequest, pb.LoginReply])
	if !ok {
		return
	}
	err := stream.Send(
		&pb.LoginReply{
			Header: &pb.ReplyHeader{
				Code: 0,
				Msg:  "SUCCESS",
			},
		})
	if err != nil {
		log.Println(err)
	}
}

func LoginFailedHandler(id string) {
	RemoveWrapperData(id)
	conn, _ := LoginConnMap.LoadAndDelete(id)
	stream, ok := conn.(grpc.BidiStreamingServer[pb.LoginRequest, pb.LoginReply])
	if !ok {
		return
	}
	err := stream.Send(
		&pb.LoginReply{
			Header: &pb.ReplyHeader{
				Code: -1,
				Msg:  "login failed",
			},
		})
	if err != nil {
		log.Println(err)
	}
}
