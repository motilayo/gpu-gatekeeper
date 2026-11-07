UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
    EXEC := limactl shell ggate --
else
    EXEC :=
endif

CC ?= clang
CFLAGS ?= -O2 -g -Wall

.PHONY: all build-mock generate build-agent test-e2e test-k3s clean

all: build-mock generate build-agent

build-mock:
	$(EXEC) mkdir -p build
	$(EXEC) $(CC) -shared -fPIC $(CFLAGS) -o build/libcuda.so mock/libcuda_mock.c
	$(EXEC) $(CC) $(CFLAGS) -o build/workload_sim mock/workload_sim.c -Lbuild -lcuda -Wl,-rpath,'$$ORIGIN'

generate:
	$(EXEC) go generate ./internal/bpf/...

build-agent: generate
	$(EXEC) mkdir -p build
	$(EXEC) go build -o build/ggate-agent ./cmd/ggate-agent

test-e2e: all
	$(EXEC) bash ./scripts/test-e2e.sh

test-k3s: all
	$(EXEC) bash ./scripts/test-k3s.sh

clean:
	rm -rf build
