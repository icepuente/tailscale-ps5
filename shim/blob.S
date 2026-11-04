/* The Go executable, embedded verbatim. build.ps1 assembles this file from
 * the build directory, where it has copied the Go output to go.elf. */

	.section .rodata
	.balign 16
	.global goshim_elf
goshim_elf:
	.incbin "go.elf"
	.global goshim_elf_end
goshim_elf_end:
