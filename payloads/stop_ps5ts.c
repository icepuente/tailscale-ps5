/* Stop a running ps5ts, however it was started (elfldr or etaHEN autostart).
 *
 * 1. Create /system_tmp/ps5ts.stop; ps5ts checks for it every 3 s and exits
 *    cleanly, removing its pid file.
 * 2. If the pid in /system_tmp/ps5ts.pid is still running after 15 s (a hung
 *    ps5ts), SIGKILL it. /system_tmp is cleared at boot, so that pid always
 *    belongs to a ps5ts started since the last boot; the process name is not
 *    checked because etaHEN names autostarted payloads differently.
 *
 * Build with -DTARGET_PID=N to kill one specific leftover pid instead
 * (only if its process name is "payload.elf"). */

#include <sys/types.h>
#include <sys/sysctl.h>
#include <sys/user.h>
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define PID_PATH  "/system_tmp/ps5ts.pid"
#define STOP_PATH "/system_tmp/ps5ts.stop"

/* Returns 1 and the process name if pid is running, else 0. Walks the whole
 * process list using each record's own ki_structsize, as Sony's kinfo_proc
 * size may differ from FreeBSD's. */
static int
running(int pid, char *comm, size_t commlen) {
  int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PROC, 0};
  size_t size = 0;
  unsigned char *buf;
  int found = 0;

  if (sysctl(mib, 4, NULL, &size, NULL, 0) || !size || size > 16 * 1024 * 1024) return 0;
  size += size / 4;
  if (!(buf = malloc(size))) return 0;
  if (!sysctl(mib, 4, buf, &size, NULL, 0)) {
    for (size_t off = 0; size - off >= sizeof(int);) {
      struct kinfo_proc *p = (void *)(buf + off);
      if (p->ki_structsize <= 0 || (size_t)p->ki_structsize > size - off) break;
      if (p->ki_pid == pid) {
        snprintf(comm, commlen, "%s", p->ki_comm);
        found = 1;
        break;
      }
      off += p->ki_structsize;
    }
  }
  free(buf);
  return found;
}

static int
read_pid(void) {
  int pid = 0;
  FILE *f = fopen(PID_PATH, "r");
  if (f) {
    if (fscanf(f, "%d", &pid) != 1) pid = 0;
    fclose(f);
  }
  return pid;
}

int
main(void) {
  char comm[64] = "";
  int pid, fd;

  setvbuf(stdout, NULL, _IONBF, 0);
#ifdef TARGET_PID
  pid = TARGET_PID;
  if (!running(pid, comm, sizeof comm) || strcmp(comm, "payload.elf")) {
    printf("stop_ps5ts: pid %d is not a running payload.elf (\"%s\"); nothing killed\n", pid, comm);
    return 1;
  }
  printf("stop_ps5ts: pid %d: %s\n", pid, kill(pid, SIGKILL) ? "kill failed" : "killed");
  return 0;
#endif

  pid = read_pid();
  if (pid <= 0 || !running(pid, comm, sizeof comm)) {
    printf("stop_ps5ts: no running ps5ts (pid file %s: %d)\n", PID_PATH, pid);
    return 0;
  }
  printf("stop_ps5ts: ps5ts is pid %d (\"%s\"); asking it to exit\n", pid, comm);
  if ((fd = open(STOP_PATH, O_CREAT | O_WRONLY, 0644)) >= 0) close(fd);
  for (int i = 0; i < 15; i++) {
    sleep(1);
    if (!running(pid, comm, sizeof comm)) {
      printf("stop_ps5ts: ps5ts exited after %d s\n", i + 1);
      return 0;
    }
  }
  printf("stop_ps5ts: still running after 15 s; killing pid %d\n", pid);
  if (kill(pid, SIGKILL)) {
    printf("stop_ps5ts: kill failed, errno %d\n", errno);
    return 1;
  }
  unlink(STOP_PATH);
  unlink(PID_PATH);
  printf("stop_ps5ts: killed\n");
  return 0;
}
