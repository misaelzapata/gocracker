#!/bin/bash
set -e

echo "Starting gocracker with iperf3 server..."
# Example command - in real usage the user would pass the compiled kernel
GOCRACKER_BIN="../../gocracker"
IPERF_PORT=5201

# Boot an Alpine VM with iperf3
$GOCRACKER_BIN run --image alpine:latest \
  --kernel "../../artifacts/kernels/gocracker-guest-standard-vmlinux" \
  --mem 512 --cpus 2 \
  --net auto \
  --cmd "apk add --no-cache iperf3 && iperf3 -s -p $IPERF_PORT" &
VM_PID=$!

echo "Waiting for VM to boot (10s)..."
sleep 10

# Depending on `--net auto`, IP might be 192.168.127.2 or similar
# The user needs to verify the exact IP, assuming 192.168.127.2 for auto NAT
GUEST_IP="192.168.127.2"

echo "Running iperf3 client from host..."
iperf3 -c $GUEST_IP -p $IPERF_PORT -t 10 || echo "Failed to connect to iperf3 server, please check IP/Port"

echo "Cleaning up..."
kill $VM_PID
