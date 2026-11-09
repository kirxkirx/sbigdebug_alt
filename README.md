# sbigdiag — SBIG STX/STXL camera diagnostics (Windows)

A single Windows `.exe` that talks **directly to the SBIG Universal Driver**
(`SBIGUDrv.dll`, the same driver NINA, CCDOps and MaxIm use). It does three things:

1. Prints what the driver and the camera report about themselves.
2. Takes test frames and analyses each one in the console as it arrives.
3. Runs experiments that change one driver setting at a time, and puts every changed setting back.

I wrote it for an **STXL-11002** with this fault: the first frame after power-on is
fine (or has a bright stripe at the bottom). In the next frame the lower part of the
image turns bright above a sharp horizontal boundary. Every frame after that is
almost fully saturated (bias frame means 10 429 → 57 213 → 56 564 ADU). A power
cycle does not cure it, and CCDOps behaves the same as NINA.

## Quick start

1. Download **`sbigdiag-win64.exe`** from the
   [latest release](https://github.com/kirxkirx/sbigdiag/releases/latest) into a folder you can write to.
2. **Disconnect the camera in NINA** (or close NINA, CCDOps and MaxIm). Only one program can own the camera.
3. Double-click the exe and choose **3 (full diagnostic, about 10 minutes)**. Enter alone also selects option 3.
4. When it finishes, send the whole `sbigdiag_<date>_<time>` folder that appears next to the exe.
   If the folder is too large, send everything except the `.fits` files.

Menu options:

| | What it does | Time |
|---|---|---|
| 1 | Camera and driver information only (no exposures) | 30 s |
| 2 | Information + 5 bias frames | 2 min |
| 3 | **Full diagnostic**: information + all experiments below | ~10 min |
| 4 | Custom series: you choose exposure, binning, subframe and temporary driver settings | – |
| 5 | Analyse FITS files you already have (for example the bad NINA frames). No camera needed; you can also drag & drop FITS files onto the exe | – |
| 6 | Demo with a simulated faulty camera | 1 min |

Use `sbigdiag-win32.exe` only if the 64-bit exe cannot load `SBIGUDrv.dll`.

## What it reports

- **Windows and driver files**:
  - Windows build (shows whether this is Windows 11).
  - Every `SBIGUDrv.dll` on the PC (System32, SysWOW64, NINA and CCDOps folders) with file version and 32/64-bit.
  - SBIG kernel drivers and their versions.
  - SBIG USB devices Windows knows about (VID 0x0D97).
  - Memory-integrity/HVCI and vulnerable-driver-blocklist state (Microsoft is blocking the legacy USBIO driver on Windows 11).
  - SBIG registry settings.
- **Driver**: version strings, USB camera list, link status, including the **communication-failure counter**.
- **Camera**:
  - Name, firmware, readout modes, gain and pixel size; serial number; capability bits.
  - **Customer options stored inside the camera**: CCD preflash, Vdd-normally-off, window heater, overscan. These survive power cycles, so they are prime suspects for a fault that a power cycle does not cure.
  - Temperatures, cooler and fan power.
  - The **power-supply-low bit**.
  - The camera's **own bias measurement** (`CC_READ_OFFSET`).
- **All 23 driver control parameters** (`DCP_*`). Any value that differs from the documented default is flagged.

## What it measures in each frame

The analysis works on raw 16-bit data:

- Basic statistics: min, max, mean, median, robust sigma, percentiles, and the fractions of saturated and zero pixels.
- **Row profile**: finds the **boundary rows** where the level jumps and lists the segments between them.
- The first saturated row.
- Medians of the left/right halves (dual-output CCD) and of even/odd columns.
- A **bit-level histogram**: stuck A/D bits mean a digital data-path fault.
- Rows that are exact copies of the previous row (a buffer or transfer fault).
- **Timing**: per-line readout time, integration time, CCD and heatsink temperature.
- The link communication-failure counter, before and after each frame.

A 64×16 ASCII map of each frame is printed on an absolute ADU scale, so a saturated
region is visible straight away in the console. Each frame is also saved as FITS,
PNG (left half: absolute log scale; right half: per-frame stretch) and a per-row CSV.
`summary.json` and `frames.csv` collect everything in one place.

## Experiments in the full test

Each experiment changes one thing. Every setting is **read back after it is
changed** and **restored afterwards**, and the restore is verified. Settings are
also restored at the end of the run and on Ctrl+C or window close.

| Experiment | Question it answers |
|---|---|
| 4 bias frames (baseline) | Does the fault reproduce? Does it develop after the first readout? |
| 5 s dark | Does exposure length matter? |
| 2×2 and 3×3 binning | Is there a readout mode that still works? Is the fault tied to clocking or readout? |
| top / bottom 200-row subframes | Is the damage tied to the position on the CCD, or to how long the readout runs? |
| `DCP_VDD_OPTIMIZED = 0` | Is the driver's Vdd (output amplifier supply) switching involved? By default Vdd is lowered only for exposures ≥ 3 s. |
| `START_SKIP_VDD` flag | Keeps Vdd on during a 5 s integration. |
| `DCP_USB_FIFO_ENABLE = 0` | Is the USB FIFO or pipelining data path involved? |
| `DCP_VERT_FLUSH_CONTROL_ENABLE = 0` | Is background vertical flushing of the KAI CCD involved? |
| `DCP_OVERSCAN_ROWSCOLS = 1` | Are pixels that never held charge also saturated? If yes, the fault is in the amplifier/analog chain/A-D, not charge on the CCD. |
| any `DCP_*` not at its documented default | Does resetting it help? |
| camera customer options → factory (only if preflash or Vdd-off is set) | Is a stored camera option the cause? Restored afterwards. |
| 2 ms pause per line | Does readout timing matter? |
| re-link (close and reopen the device) | Does a software re-initialisation clear the state? |
| 90 s rest | Does the fault decay with idle time (thermal or supply recovery)? |
| final baseline | Confirms everything is back to normal. |

At the end it prints a table of all frames and some heuristic observations. For
example, "binning 2×2 works while the baseline fails", or "the camera's own offset
measurement is at full scale, which points to the analog chain".

Notes:

- Driver control parameters belong to the process, not to the camera. Changing them
  here does not affect NINA, and they vanish when sbigdiag exits.
- The camera "customer options" are stored in the camera. sbigdiag only changes them
  when preflash or Vdd-off is set, and it restores them.
- Cooling, fan and filter wheel are never touched.

## Command line

```
sbigdiag-win64.exe -full                    full test without the menu
sbigdiag-win64.exe -quick -n 10             10 bias frames
sbigdiag-win64.exe -custom "exp=5,bin=1,dark=1,top=0,height=200" -n 3
sbigdiag-win64.exe -custom "exp=0" -set "VDD_OPTIMIZED=0,USB_FIFO_ENABLE=0" -n 5
sbigdiag-win64.exe -info                    information only
sbigdiag-win64.exe bad1.fits bad2.fits      analyse existing files
  -dll <path>      use a specific SBIGUDrv.dll (e.g. a different driver version)
  -device usb1     choose a USB camera; -device eth -ip a.b.c.d for Ethernet
  -driverlog       also write the SBIG driver's own debug log
  -nofits          do not save FITS files
  -out <dir>       output folder
```

`-dll` lets you compare driver versions without installing them. Copy another
`SBIGUDrv.dll` version into a folder and point `-dll` at it. The 64-bit exe needs a
64-bit DLL, and the 32-bit exe needs a 32-bit DLL.

## How it is built and checked

- Pure Go, no cgo. The DLL is loaded at run time and nothing from SBIG is bundled.
- The parameter and result structures are laid out byte by byte in `abi.go`. CI
  compiles 183 `_Static_assert`s of those offsets and sizes against the vendor
  `sbigudrv.h`, for both `i686` and `x86_64` Windows (`tools/abicheck`).
- CI also does the following:
  - Runs the whole program against a built-in simulated STXL that reproduces the fault.
  - Smoke-tests both exes on a Windows runner: simulator, FITS analysis, and a clean error when the driver is missing.
  - Publishes releases on `v*` tags.
