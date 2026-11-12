/* Linux native entry: no libc, loader, constructors, getenv or fixed fd slots.
 * The payload path is part of this image's build, never workload input. */
typedef unsigned long U;
#ifndef GUEST_PATH
#define GUEST_PATH "/usr/local/libexec/iterion-execenv-guest"
#endif
#if defined(__x86_64__)
#define SYS_WRITE 1
#define SYS_CLOSE 3
#define SYS_EXIT 60
#define SYS_MEMFD 319
#define SYS_FCHMOD 91
#define SYS_SEEK 8
#define SYS_FCNTL 72
#define SYS_OPENAT 257
#define SYS_EXECVEAT 322
static long call(long n,long a,long b,long c,long d,long e,long f) {
 long r; register long r10 __asm__("r10")=d, r8 __asm__("r8")=e, r9 __asm__("r9")=f;
 __asm__ volatile("syscall":"=a"(r):"a"(n),"D"(a),"S"(b),"d"(c),"r"(r10),"r"(r8),"r"(r9):"rcx","r11","memory"); return r;
}
__asm__(".global _start\n_start:\nmov %rsp,%rdi\nandq $-16,%rsp\ncall entry\n");
#elif defined(__aarch64__)
#define SYS_WRITE 64
#define SYS_CLOSE 57
#define SYS_EXIT 93
#define SYS_MEMFD 279
#define SYS_FCHMOD 52
#define SYS_SEEK 62
#define SYS_FCNTL 25
#define SYS_OPENAT 56
#define SYS_EXECVEAT 281
static long call(long n,long a,long b,long c,long d,long e,long f) {
 register long x8 __asm__("x8")=n, x0 __asm__("x0")=a, x1 __asm__("x1")=b, x2 __asm__("x2")=c, x3 __asm__("x3")=d, x4 __asm__("x4")=e, x5 __asm__("x5")=f;
 __asm__ volatile("svc 0":"+r"(x0):"r"(x8),"r"(x1),"r"(x2),"r"(x3),"r"(x4),"r"(x5):"memory","cc"); return x0;
}
__asm__(".global _start\n_start:\nmov x0, sp\nbl entry\n");
#else
#error Linux capsule supports only amd64 and arm64
#endif
#define EINTR 4
#define AT_FDCWD (-100)
#define O_PATH 010000000
#define O_CLOEXEC 02000000
#define F_SETFD 2
#define F_DUPFD_CLOEXEC 1030
#define AT_EMPTY_PATH 0x1000
static long env_fd=-1, image_fd=-1, payload_fd=-1;
static U length(const char *p) { U n=0; while(p[n]) n++; return n; }
static void close_owned(long *fd) { if (*fd>=0) { call(SYS_CLOSE,*fd,0,0,0,0,0); *fd=-1; } }
__attribute__((noreturn)) static void fail(const char *phase) {
 const char *prefix="exec environment transport: native: ";
 close_owned(&payload_fd); close_owned(&image_fd); close_owned(&env_fd);
 call(SYS_WRITE,2,(long)prefix,length(prefix),0,0,0); call(SYS_WRITE,2,(long)phase,length(phase),0,0,0); call(SYS_WRITE,2,(long)"\n",1,0,0,0);
 call(SYS_EXIT,125,0,0,0,0,0); for(;;) {}
}
/* Allocate above stdio without overwriting any existing descriptor. If the
 * kernel used a previously closed stdio slot, restore that slot to closed. */
static long above_stdio(long fd) {
 if(fd<0) fail("allocate");
 if(fd>=3) return fd;
 long moved=call(SYS_FCNTL,fd,F_DUPFD_CLOEXEC,3,0,0,0);
 long closed=call(SYS_CLOSE,fd,0,0,0,0,0);
 if(moved<0 || closed<0) { if(moved>=0) call(SYS_CLOSE,moved,0,0,0,0,0); fail("allocate"); }
 return moved;
}
static void write_all(long fd,const char *p,U n) {
 while(n) {
  long k=call(SYS_WRITE,fd,(long)p,(long)n,0,0,0);
  if(k==-EINTR) continue;
  if(k<=0 || (U)k>n) fail("capture");
  p+=k; n-=(U)k;
 }
}
static void append_fd(char *p,long fd) {
 U i=length(p),n=(U)fd,k=0; char reversed[32];
 do { reversed[k++]=(char)('0'+n%10); n/=10; } while(n);
 while(k) { p[i++]=reversed[--k]; }
 p[i]=0;
}
__attribute__((used,noreturn)) void entry(U *stack) {
 U argc=stack[0]; char **argv=(char **)(stack+1), **env=argv+argc+1;
 env_fd=above_stdio(call(SYS_MEMFD,(long)"iterion-execenv",1,0,0,0,0));
 if(call(SYS_FCHMOD,env_fd,0600,0,0,0,0)<0) fail("mode");
 image_fd=above_stdio(call(SYS_OPENAT,AT_FDCWD,(long)"/proc/self/exe",O_PATH|O_CLOEXEC,0,0,0));
 for(char **p=env;*p;p++) write_all(env_fd,*p,length(*p)+1);
 if(call(SYS_SEEK,env_fd,0,0,0,0,0)<0) fail("seek");
 payload_fd=above_stdio(call(SYS_OPENAT,AT_FDCWD,(long)GUEST_PATH,O_PATH|O_CLOEXEC,0,0,0));
 char env_control[64]="IENV_NATIVE_ENV_FD=", image_control[64]="IENV_NATIVE_IMAGE_FD=";
 append_fd(env_control,env_fd); append_fd(image_control,image_fd);
 char *clean[]={env_control,image_control,0}; char *guest_argv[]={GUEST_PATH,0};
 if(call(SYS_FCNTL,env_fd,F_SETFD,0,0,0,0)<0 || call(SYS_FCNTL,image_fd,F_SETFD,0,0,0,0)<0) fail("handoff");
 call(SYS_EXECVEAT,payload_fd,(long)"",(long)guest_argv,(long)clean,AT_EMPTY_PATH,0);
 fail("exec");
}
