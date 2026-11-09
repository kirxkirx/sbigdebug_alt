/* Minimal stand-in for <Windows.h>: sbigudrv.h only needs LARGE_INTEGER. */
#pragma once
typedef long long LONGLONG;
typedef union _LARGE_INTEGER { struct { unsigned long LowPart; long HighPart; } u; LONGLONG QuadPart; } LARGE_INTEGER;
