#!/bin/bash
set -e

echo "Starting gocracker with fio benchmark..."

GOCRACKER_BIN="../../gocracker"

# Boot an Alpine VM with fio
$GOCRACKER_BIN run --image alpine:latest \
  --kernel "../../artifacts/kernels/gocracker-guest-standard-vmlinux" \
  --mem 1024 --cpus 2 \
  --cmd "apk add --no-cache fio && fio --name=randwrite --ioengine=libaio --iodepth=64 --rw=randwrite --bs=4k --direct=1 --size=1G --numjobs=4 --runtime=30 --group_reporting" \
  --wait
