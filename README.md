# wrapper-manager
A tool for managing multiple Wrapper instances

Support linux x86_64 and arm64 arch

## Features
- Multi-instance management
- Add accounts at runtime (support 2FA)
- Multi-connection decryption
- gRPC API
- Get lyrics without an account
- Automatic region detection

## Usage
```shell
Usage of ./wrapper-manager:
  -debug
        enable debug output
  -host string
        host of gRPC server (default "localhost")
  -mirror
        use mirror to download wrapper and file (for Chinese users)
  -port int
        port of gRPC server (default 8080)
  -proxy string
        proxy for wrapper and manager
```

## Deploy
For Chinese users: Please uncomment the sixth line of `Dockerfile` and configure the mirror and proxy in `docker-compose.yml`
```shell
git clone https://github.com/WorldObservationLog/wrapper-manager
cd wrapper-manager
nano docker-compose.yml
docker compose up
```

## Login
You can use [WorldObservationLog/AppleMusicDecrypt](https://github.com/WorldObservationLog/AppleMusicDecrypt) `tools/login.py` to log in, or use tools such as Postman to import `proto/manager.proto` to log in. The process is as follows:
![flowchart.png](/flowchart.png)