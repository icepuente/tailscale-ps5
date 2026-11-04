/* goshim: run an embedded static freebsd/amd64 Go executable inside a PS5
 * payload process.
 *
 * The SDK crt has already run before main: the process has elevated ucred
 * capabilities and may issue syscalls from any address, which the Go runtime
 * needs because it makes raw syscalls instead of calling libkernel.
 *
 * What remains is what a FreeBSD kernel would do on execve: map the PT_LOAD
 * segments, then enter the program with rdi pointing at argc, argv, envp and
 * auxv (Go's _rt0_amd64_freebsd reads them through rdi, not rsp).
 */

#include <errno.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

/* Provided by blob.S. */
extern const unsigned char goshim_elf[];
extern const unsigned char goshim_elf_end[];

#define PT_LOAD    1
#define PT_DYNAMIC 2
#define PT_INTERP  3
#define PF_X 1
#define PF_W 2
#define PF_R 4

#define AT_NULL   0
#define AT_PAGESZ 6

#define GO_STACK_SIZE (8ul << 20)

/* Dead-man switch: the whole process exits this many seconds after Go
 * starts, so a hung probe cannot linger on the console. 0 disables it. */
#ifndef GOSHIM_DEADLINE_SECONDS
#define GOSHIM_DEADLINE_SECONDS 90
#endif

typedef struct {
  unsigned char e_ident[16];
  uint16_t e_type;
  uint16_t e_machine;
  uint32_t e_version;
  uint64_t e_entry;
  uint64_t e_phoff;
  uint64_t e_shoff;
  uint32_t e_flags;
  uint16_t e_ehsize;
  uint16_t e_phentsize;
  uint16_t e_phnum;
  uint16_t e_shentsize;
  uint16_t e_shnum;
  uint16_t e_shstrndx;
} elf64_ehdr;

typedef struct {
  uint32_t p_type;
  uint32_t p_flags;
  uint64_t p_offset;
  uint64_t p_vaddr;
  uint64_t p_paddr;
  uint64_t p_filesz;
  uint64_t p_memsz;
  uint64_t p_align;
} elf64_phdr;

/* Strings handed to the Go program. GOTRACEBACK=all prints every goroutine
 * on a fatal error and then exits. Not "crash": its SIGABRT left the
 * process hanging on the PS5 instead of terminating it.
 * asyncpreemptoff=1: the PS5 kernel does not let a signal handler redirect
 * the interrupted thread, so async preemption cannot work and would only
 * waste signals. */
/* Absolute so the patched os.Executable can report it (a payload has no
 * executable file of its own). */
static char *const go_argv[] = {"/data/ps5_tailscale/goprogram", 0};
static char *const go_envp[] = {
  "HOME=/data/ps5_tailscale",
  "GOTRACEBACK=all",
  "GODEBUG=asyncpreemptoff=1",
  0
};

static void
say(const char *msg) {
  fputs(msg, stdout);
  fputc('\n', stdout);
  fflush(stdout);
}

/* Runs on its own libkernel thread, which the Go runtime never touches.
 * It is created with every signal blocked, so Go's process-wide handlers
 * never run on it. */
static void *
deadline_thread(void *arg) {
  unsigned left = GOSHIM_DEADLINE_SECONDS;
  (void)arg;
  while (left > 0) {
    left = sleep(left);
  }
  fputs("\ngoshim: deadline reached, ending process\n", stdout);
  fflush(stdout);
  /* FreeBSD exit(2), syscall 1: ends every thread in the process. */
  __asm__ volatile("syscall" : : "a"(1), "D"(124) : "rcx", "r11", "memory");
  return 0;
}

static int
prot_of(uint32_t flags) {
  return ((flags & PF_R) ? PROT_READ : 0) |
         ((flags & PF_W) ? PROT_WRITE : 0) |
         ((flags & PF_X) ? PROT_EXEC : 0);
}

int
main(void) {
  const elf64_ehdr *eh = (const elf64_ehdr *)goshim_elf;
  size_t blob_size = (size_t)(goshim_elf_end - goshim_elf);
  const elf64_phdr *ph;
  uint64_t page = (uint64_t)getpagesize();
  uint64_t lo = UINT64_MAX;
  uint64_t hi = 0;
  unsigned char *base;
  size_t span;

  setvbuf(stdout, 0, _IONBF, 0);
  printf("goshim: embedded ELF %zu bytes, page size %lu\n", blob_size, page);

  if (blob_size < sizeof(*eh) || memcmp(eh->e_ident, "\177ELF", 4) ||
      eh->e_ident[4] != 2 || eh->e_machine != 62) {
    say("goshim: not an x86-64 ELF64");
    return 1;
  }
  if (eh->e_type != 2) {
    printf("goshim: e_type %u, expected ET_EXEC (build without -buildmode=pie)\n", eh->e_type);
    return 1;
  }
  if (eh->e_phoff + (uint64_t)eh->e_phnum * sizeof(*ph) > blob_size) {
    say("goshim: program headers out of range");
    return 1;
  }
  ph = (const elf64_phdr *)(goshim_elf + eh->e_phoff);

  for (int i = 0; i < eh->e_phnum; i++) {
    if (ph[i].p_type == PT_DYNAMIC || ph[i].p_type == PT_INTERP) {
      say("goshim: Go ELF is dynamically linked; build with CGO_ENABLED=0");
      return 1;
    }
    if (ph[i].p_type != PT_LOAD) {
      continue;
    }
    if (ph[i].p_offset + ph[i].p_filesz > blob_size || ph[i].p_filesz > ph[i].p_memsz) {
      say("goshim: PT_LOAD out of range");
      return 1;
    }
    if (ph[i].p_vaddr < lo) lo = ph[i].p_vaddr;
    if (ph[i].p_vaddr + ph[i].p_memsz > hi) hi = ph[i].p_vaddr + ph[i].p_memsz;
  }
  if (lo >= hi) {
    say("goshim: no PT_LOAD segments");
    return 1;
  }
  lo &= ~(page - 1);
  hi = (hi + page - 1) & ~(page - 1);
  span = hi - lo;

  /* Ask for the link address as a hint, not MAP_FIXED, so nothing already
   * mapped in this process can be clobbered. */
  base = mmap((void *)lo, span, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
  if (base == MAP_FAILED) {
    printf("goshim: mmap %#lx bytes failed: %s\n", span, strerror(errno));
    return 1;
  }
  if ((uint64_t)base != lo) {
    printf("goshim: wanted %#lx, kernel gave %p; pick another -T address\n", lo, base);
    munmap(base, span);
    return 1;
  }
  printf("goshim: image mapped at %#lx-%#lx\n", lo, hi);

  for (int i = 0; i < eh->e_phnum; i++) {
    if (ph[i].p_type == PT_LOAD) {
      memcpy((void *)ph[i].p_vaddr, goshim_elf + ph[i].p_offset, ph[i].p_filesz);
    }
  }

  /* Segments may share a page when the Go linker rounds to 4 KiB and the
   * kernel uses 16 KiB, so protect page by page with the union of the
   * flags of every segment touching that page. Runs of equal protection
   * go to the kernel as one call. The SDK mprotect raises PROT_EXEC
   * through kernel memory writes; plain mprotect cannot. */
  {
    uint64_t run_start = lo;
    int run_prot = -1;
    for (uint64_t p = lo; p <= hi; p += page) {
      int prot = 0;
      if (p < hi) {
        for (int i = 0; i < eh->e_phnum; i++) {
          if (ph[i].p_type == PT_LOAD && ph[i].p_vaddr < p + page &&
              ph[i].p_vaddr + ph[i].p_memsz > p) {
            prot |= prot_of(ph[i].p_flags);
          }
        }
      }
      if (p == hi || prot != run_prot) {
        if (run_prot >= 0 && mprotect((void *)run_start, p - run_start, run_prot)) {
          printf("goshim: mprotect %#lx+%#lx prot %d failed: %s\n",
                 run_start, p - run_start, run_prot, strerror(errno));
          return 1;
        }
        run_start = p;
        run_prot = prot;
      }
    }
  }
  say("goshim: segment protections applied");

  /* A fresh stack with the FreeBSD execve layout at its top:
   * argc, argv..., 0, envp..., 0, auxv pairs, AT_NULL. */
  {
    unsigned char *stack = mmap(0, GO_STACK_SIZE, PROT_READ | PROT_WRITE,
                                MAP_PRIVATE | MAP_ANON, -1, 0);
    uint64_t *sp;
    uint64_t *block;
    int argc = 0;
    int envc = 0;
    int words;

    if (stack == MAP_FAILED) {
      printf("goshim: stack mmap failed: %s\n", strerror(errno));
      return 1;
    }
    while (go_argv[argc]) argc++;
    while (go_envp[envc]) envc++;
    words = 1 + argc + 1 + envc + 1 + 4;
    block = (uint64_t *)((uintptr_t)(stack + GO_STACK_SIZE - (size_t)words * 8 - 64) & ~(uintptr_t)15);

    sp = block;
    *sp++ = (uint64_t)argc;
    for (int i = 0; i < argc; i++) *sp++ = (uint64_t)go_argv[i];
    *sp++ = 0;
    for (int i = 0; i < envc; i++) *sp++ = (uint64_t)go_envp[i];
    *sp++ = 0;
    *sp++ = AT_PAGESZ;
    *sp++ = page;
    *sp++ = AT_NULL;
    *sp++ = 0;

    if (GOSHIM_DEADLINE_SECONDS > 0) {
      pthread_t tid;
      sigset_t all;
      sigset_t old;
      int err;
      sigfillset(&all);
      pthread_sigmask(SIG_SETMASK, &all, &old);
      err = pthread_create(&tid, 0, deadline_thread, 0);
      pthread_sigmask(SIG_SETMASK, &old, 0);
      if (err) {
        printf("goshim: deadline thread failed: %s; not starting Go\n", strerror(err));
        return 1;
      }
      printf("goshim: process ends after %d s regardless of Go\n", GOSHIM_DEADLINE_SECONDS);
    }

    printf("goshim: entering Go at %#lx\n", eh->e_entry);

    /* Go installs its own fs base and never returns here. */
    __asm__ volatile(
      "mov %%rdi, %%rsp\n"
      "xor %%ebp, %%ebp\n"
      "jmp *%%rax\n"
      :
      : "D"(block), "a"(eh->e_entry)
      : "memory");
    __builtin_unreachable();
  }
}
