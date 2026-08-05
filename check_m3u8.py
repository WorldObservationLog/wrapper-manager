#!/usr/bin/env python3
"""诊断 wrapper 的 M3U8 接口是否可用。

按 wrapper-manager 的 M3U8 协议：先写 1 字节的 adamId 长度，再写 adamId，
然后读一行(以 \n 结尾)作为响应。能拿到非空响应说明 M3U8 正常工作。

用法：
  python check_m3u8.py <port> <adamId> [--timeout 秒]

例：
  python check_m3u8.py 62884 6787576394
  python check_m3u8.py 62884 6787576394 --timeout 15
"""
import socket
import sys
import time


def check_m3u8(port: int, adam_id: str, timeout: float) -> None:
    print(f"[*] 目标: 127.0.0.1:{port}  adamId={adam_id}  timeout={timeout}s")
    sock = socket.create_connection(("127.0.0.1", port), timeout=timeout)
    sock.settimeout(timeout)
    try:
        # 1 字节长度前缀 + adamId（与 manager 的 GetM3U8 一致）
        payload = bytes([len(adam_id)]) + adam_id.encode()
        t0 = time.time()
        sock.sendall(payload)
        print(f"[+] 已发送 {len(payload)} 字节: len={len(adam_id)} adamId={adam_id}")

        # 读一行响应
        buf = bytearray()
        while True:
            chunk = sock.recv(4096)
            if not chunk:
                break
            buf += chunk
            if b"\n" in buf:
                break

        elapsed = time.time() - t0
        if not buf:
            print("[!] 响应为空（连接被关闭，无数据返回）")
            sys.exit(2)

        first_line = buf.split(b"\n", 1)[0]
        text = first_line.decode("utf-8", errors="replace").strip()
        is_m3u8 = "EXTM3U" in text or "http" in text.lower() or "#EXT" in text
        print(f"[+] 收到响应 {elapsed:.3f}s, {len(buf)} 字节")
        print("-" * 60)
        print(text[:2000])
        print("-" * 60)
        if is_m3u8:
            print("[OK] 响应包含 M3U8 特征（#EXT / http），wrapper M3U8 工作正常")
            sys.exit(0)
        else:
            print("[!] 响应不像是 M3U8 内容，请人工确认上面的输出")
            sys.exit(1)
    finally:
        sock.close()


def main() -> None:
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    kwargs = {}
    for i, a in enumerate(sys.argv[1:]):
        if a == "--timeout" and i + 1 < len(sys.argv[1:]):
            kwargs["timeout"] = float(sys.argv[i + 2])

    if len(args) < 2:
        print(__doc__)
        sys.exit(2)

    port = int(args[0])
    adam_id = args[1]
    timeout = kwargs.get("timeout", 10.0)
    check_m3u8(port, adam_id, timeout)


if __name__ == "__main__":
    main()
