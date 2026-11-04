/* Harmless test payload: prints one line and exits. Used to check that
 * payloads sent to elfldr over the tailnet (ps5:9021) arrive and run. */

#include <stdio.h>
#include <unistd.h>

int
main(void) {
  printf("hello from a payload, pid %d\n", getpid());
  return 0;
}
