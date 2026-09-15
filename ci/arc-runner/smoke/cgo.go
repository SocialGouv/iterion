package smoke

/*
#include <stdlib.h>
*/
import "C"

func absolute(n int) int { return int(C.abs(C.int(n))) }
