package bpf

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64,amd64 -cc clang -cflags "-O2 -g -Wall -Werror -I../../bpf" ggate ../../bpf/ggate.bpf.c
