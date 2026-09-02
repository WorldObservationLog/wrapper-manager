# wrapper-manager QEMU guest

Assets for booting the whole wrapper-manager inside a QEMU Linux guest so it
runs on Windows / macOS / any Linux host (see the parent README).

## Layout

```text
guest/
├── build.sh     # builds wrapper-manager-initramfs.cpio.gz (+ optional data.img)
├── init         # guest init script (busybox sh)
└── modules/     # kernel modules reused from the upstream wrapper initramfs
    ├── e1000.ko
    └── qemu_fw_cfg.ko
```

`overlay/` is a build output (gitignored): build.sh rebuilds it from scratch
each run by staging the static wrapper-manager binary, static busybox, the
host glibc runtime (for the dynamically-linked `wrapper-lite-rootless`), CA
certificates and the kernel modules.

## Build

```shell
# requires: go, cpio, gzip, e2fsprogs, glibc + ca-certificates (any Debian/Ubuntu)
./guest/build.sh out                  # initramfs only
CREATE_DATA_IMG=1 ./guest/build.sh out   # also create a fresh 512MB data.img
```

## Kernel

`vmlinuz-lite-qemu` is **not** built here. It is the kernel produced by the
upstream [WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper)
`build-lite` workflow and is distributed in its `qemu-assets` artifact:

```shell
curl -sL -o qemu-assets.zip \
  "https://nightly.link/WorldObservationLog/wrapper/workflows/build-lite/lite/qemu-assets.zip"
unzip -o qemu-assets.zip vmlinuz-lite-qemu -d out/
```

Copy it next to the initramfs (the launcher's `--assets-dir`, default
`<exe>/guest`).

## What the guest does on boot (init)

1. Mounts proc/sys/dev, loads `e1000.ko`, configures static NAT networking
   (10.0.2.15, DNS 10.0.2.3) and writes `/etc/resolv.conf`.
2. Mounts the ext4 data disk (`/dev/vda` → `/data`); formats it with
   `mke2fs` on first boot when it is a raw empty image.
3. Executes `/wrapper-manager -host 0.0.0.0 -port 8080` from `/data`, so all
   manager state (wrapper-lite payload, accounts, tokens) persists in
   `data.img`. Optional manager args can be supplied via qemu fw_cfg
   (`manager_args`) or the kernel cmdline (`manager_args_b64=`).
