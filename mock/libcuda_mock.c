#include <stddef.h>

typedef int CUresult;
typedef void* CUfunction;
typedef unsigned long long CUdeviceptr;

#define CUDA_SUCCESS 0

__attribute__((noinline, used, visibility("default")))
CUresult cuLaunchKernel(CUfunction f,
                        unsigned int gridDimX,
                        unsigned int gridDimY,
                        unsigned int gridDimZ,
                        unsigned int blockDimX,
                        unsigned int blockDimY,
                        unsigned int blockDimZ,
                        unsigned int sharedMemBytes,
                        void *hStream,
                        void **kernelParams,
                        void **extra)
{
    asm volatile("" : : : "memory");
    return CUDA_SUCCESS;
}

__attribute__((noinline, used, visibility("default")))
CUresult cuLaunchKernelEx(const void *config,
                          CUfunction f,
                          void **kernelParams,
                          void **extra)
{
    asm volatile("" : : : "memory");
    return CUDA_SUCCESS;
}

static unsigned long long next_dptr = 0xdead0000ULL;

__attribute__((noinline, used, visibility("default")))
CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize)
{
    asm volatile("" : : : "memory");
    if (dptr) {
        *dptr = __sync_fetch_and_add(&next_dptr, 0x10000ULL);
    }
    return CUDA_SUCCESS;
}

__attribute__((noinline, used, visibility("default")))
CUresult cuMemFree_v2(CUdeviceptr dptr)
{
    asm volatile("" : : : "memory");
    return CUDA_SUCCESS;
}
