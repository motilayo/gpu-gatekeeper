#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <sys/stat.h>
#include <sys/types.h>

typedef int CUresult;
typedef void* CUfunction;
typedef unsigned long long CUdeviceptr;
#define CUDA_SUCCESS 0

extern CUresult cuLaunchKernel(CUfunction f,
                               unsigned int gridDimX,
                               unsigned int gridDimY,
                               unsigned int gridDimZ,
                               unsigned int blockDimX,
                               unsigned int blockDimY,
                               unsigned int blockDimZ,
                               unsigned int sharedMemBytes,
                               void *hStream,
                               void **kernelParams,
                               void **extra);

extern CUresult cuGraphLaunch(void *hGraphExec, void *hStream);
extern CUresult cuGraphAddKernelNode(void **phGraphNode,
                                     void *hGraph,
                                     const void *dependencies,
                                     size_t numDependencies,
                                     const void *nodeParams);
extern CUresult cuGraphInstantiate(void **phGraphExec,
                                   void *hGraph,
                                   void *phErrorNode,
                                   char *logBuffer,
                                   size_t bufferSize);
extern CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize);
extern CUresult cuMemFree_v2(CUdeviceptr dptr);
extern CUresult cuMemAllocAsync(CUdeviceptr *dptr, size_t bytesize, void *hStream);
extern CUresult cuMemFreeAsync(CUdeviceptr dptr, void *hStream);

struct cuda_kernel_node_params_prefix {
    void *func;
    unsigned int gridDimX;
    unsigned int gridDimY;
    unsigned int gridDimZ;
    unsigned int blockDimX;
    unsigned int blockDimY;
    unsigned int blockDimZ;
};

static unsigned long long get_cgroup_id(void) {
    FILE *fp = fopen("/proc/self/cgroup", "r");
    char cgroup_path[1024] = "/sys/fs/cgroup";
    if (fp) {
        char line[512];
        while (fgets(line, sizeof(line), fp)) {
            line[strcspn(line, "\r\n")] = '\0';
            if (strncmp(line, "0::", 3) == 0) {
                const char *rel = line + 3;
                if (strlen(rel) > 0 && strcmp(rel, "/") != 0) {
                    snprintf(cgroup_path, sizeof(cgroup_path), "/sys/fs/cgroup%s%s",
                             (rel[0] == '/') ? "" : "/", rel);
                }
                break;
            }
        }
        fclose(fp);
    }
    struct stat st;
    if (stat(cgroup_path, &st) == 0) {
        return (unsigned long long)st.st_ino;
    }
    if (stat("/sys/fs/cgroup", &st) == 0) {
        return (unsigned long long)st.st_ino;
    }
    return 0;
}

int main(int argc, char *argv[]) {
    unsigned long long iterations = 10000;
    double duration = 0.0;
    size_t vram_bytes = 67108864ULL; /* default 64 MiB */
    unsigned int grid_x = 128;
    unsigned int block_x = 256;
    int free_on_exit = 0;
    int use_graph_launch = 0;
    int use_async_vram = 0;
    int graph_nodes = 0;

    for (int i = 1; i < argc; i++) {
        if ((strcmp(argv[i], "-n") == 0 || strcmp(argv[i], "--n") == 0) && i + 1 < argc) {
            iterations = strtoull(argv[++i], NULL, 10);
        } else if ((strcmp(argv[i], "-duration") == 0 || strcmp(argv[i], "--duration") == 0) && i + 1 < argc) {
            duration = atof(argv[++i]);
        } else if ((strcmp(argv[i], "-vram") == 0 || strcmp(argv[i], "--vram") == 0) && i + 1 < argc) {
            vram_bytes = (size_t)strtoull(argv[++i], NULL, 10);
        } else if ((strcmp(argv[i], "-grid") == 0 || strcmp(argv[i], "--grid") == 0) && i + 1 < argc) {
            grid_x = (unsigned int)strtoul(argv[++i], NULL, 10);
        } else if ((strcmp(argv[i], "-block") == 0 || strcmp(argv[i], "--block") == 0) && i + 1 < argc) {
            block_x = (unsigned int)strtoul(argv[++i], NULL, 10);
        } else if (strcmp(argv[i], "-free-on-exit") == 0 || strcmp(argv[i], "--free-on-exit") == 0) {
            free_on_exit = 1;
        } else if (strcmp(argv[i], "-graph-launch") == 0 || strcmp(argv[i], "--graph-launch") == 0) {
            use_graph_launch = 1;
        } else if ((strcmp(argv[i], "-graph-nodes") == 0 || strcmp(argv[i], "--graph-nodes") == 0) && i + 1 < argc) {
            graph_nodes = atoi(argv[++i]);
        } else if (strcmp(argv[i], "-async-vram") == 0 || strcmp(argv[i], "--async-vram") == 0) {
            use_async_vram = 1;
        }
    }

    pid_t pid = getpid();
    unsigned long long cgroup_id = get_cgroup_id();
    printf("workload_sim started: pid=%d cgroup_id=%llu vram_alloc=%zu\n",
           (int)pid, cgroup_id, vram_bytes);
    fflush(stdout);

    /* Allocate VRAM once before kernel launches */
    CUdeviceptr dptr = 0;
    if (use_async_vram) {
        cuMemAllocAsync(&dptr, vram_bytes, NULL);
    } else {
        cuMemAlloc_v2(&dptr, vram_bytes);
    }

    /* Instantiate graph if graph-launch and graph-nodes > 0 */
    void *hGraphExec = (void *)0x1;
    if (use_graph_launch && graph_nodes > 0) {
        void *hGraph = (void *)(unsigned long long)getpid();
        struct cuda_kernel_node_params_prefix params = {
            .func = (void *)0x1,
            .gridDimX = grid_x,
            .gridDimY = 1,
            .gridDimZ = 1,
            .blockDimX = block_x,
            .blockDimY = 1,
            .blockDimZ = 1,
        };
        for (int k = 0; k < graph_nodes; k++) {
            void *node = NULL;
            cuGraphAddKernelNode(&node, hGraph, NULL, 0, &params);
        }
        cuGraphInstantiate(&hGraphExec, hGraph, NULL, NULL, 0);
    }

    struct timespec start, end;
    clock_gettime(CLOCK_MONOTONIC, &start);

    unsigned long long launches = 0;
    if (duration > 0.0) {
        while (1) {
            if (use_graph_launch) {
                cuGraphLaunch(hGraphExec, NULL);
            } else {
                cuLaunchKernel((CUfunction)0x1, grid_x, 1, 1, block_x, 1, 1, 0, NULL, NULL, NULL);
            }
            launches++;

            struct timespec current;
            clock_gettime(CLOCK_MONOTONIC, &current);
            double cur_elapsed = (double)(current.tv_sec - start.tv_sec) +
                                 (double)(current.tv_nsec - start.tv_nsec) / 1e9;
            if (cur_elapsed >= duration) {
                break;
            }
        }
    } else {
        for (unsigned long long i = 0; i < iterations; i++) {
            if (use_graph_launch) {
                cuGraphLaunch(hGraphExec, NULL);
            } else {
                cuLaunchKernel((CUfunction)0x1, grid_x, 1, 1, block_x, 1, 1, 0, NULL, NULL, NULL);
            }
            launches++;
        }
    }

    clock_gettime(CLOCK_MONOTONIC, &end);

    if (free_on_exit) {
        if (use_async_vram) {
            cuMemFreeAsync(dptr, NULL);
        } else {
            cuMemFree_v2(dptr);
        }
    }

    double elapsed_sec = (double)(end.tv_sec - start.tv_sec) +
                         (double)(end.tv_nsec - start.tv_nsec) / 1e9;
    double iops = (elapsed_sec > 0.0) ? ((double)launches / elapsed_sec) : 0.0;

    printf("workload_sim finished: launches=%llu elapsed_sec=%.6f iops=%.2f\n",
           launches, elapsed_sec, iops);
    fflush(stdout);

    return 0;
}
