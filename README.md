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

## The test that decides it (needs one power cycle)

Three explanations fit everything seen so far. One test separates them.

| Suspect | Why it fits | How to fix it |
|---|---|---|
| **NINA 3.2's SBIG driver code** | NINA 3.2 can send a temperature query to the SBIG driver while a frame is being read out; the driver is not thread-safe. The fix is in NINA 3.3 nightly ("Fixed missing synchronization in native … SBIG … SDK calls", commit `f475c75d9f`). A garbled command can leave the camera bad until it is power-cycled, which is why CCDOps also failed when run without a power cycle. | Update NINA to 3.3 (nightly), or use the SBIG ASCOM driver |
| **NINA's bundled old driver DLL** | NINA loads its own x64 `SBIGUDrv.dll` 4.99 Build 8 from `External\x64\SBIG`. Diffraction Limited forum cases (STXL-11002 2016, STT-8300 2017, STX-16803 2022) had saturated bias/darks from **RBI-preflash handling with driver and firmware out of step**. In two of them, once MaxIm had connected, CCDOps stayed bad until a power cycle. | Put DL's x64 `SBIGUDrv.dll` 5.1.1.0 into NINA's `External\x64\SBIG` folder |
| **Camera hardware** | DL calls the internal power module "the most common failure in these old cameras". An STXL failed only below 0 °C (frost in the chamber was suspected). The supply must deliver 12–13 V at ≥ 10 A. | Supply/cable, desiccant, or repair (DL no longer repairs STXL since Aug 2026) |

**Protocol:**

1. Close NINA so that it does not reconnect, then power-cycle the camera. While you are there,
   look at the **voltage LEDs** on the side of the camera: only green should be lit. The 10V LED
   means the cooler is shut off; the red 9V LED means operation is unreliable.
2. Back at the PC, run **`sbigdiag-win32.exe`**, option **2**. It uses the same 32-bit system DLL as CCDOps.
   - Bad frames → hardware, or a camera state that does not come from NINA. Run option 3 and send the folder.
3. If step 2 was good, run **`sbigdiag-win64.exe`**, option **2**. It uses NINA's own DLL.
   - Bad frames → NINA's old DLL is the cause (see the table for the fix).
   - Good frames → NINA 3.2's unsynchronised calls are the cause (see the table for the fix).
4. Confirm by imaging in NINA as usual. If it breaks again, the camera needs another power cycle.

The report says which DLL it loaded (path and version) at the top. Use `-dll <path>` to test any other copy.

Also note: NINA shows the firmware as "2.84", but it prints the low byte of a BCD
number in decimal. The camera's real firmware is **2.54**, the gate-array version
DL shipped in 2022 as the fix for the RBI-preflash bug, so the firmware is current.

## Quick start

1. Download **`sbigdiag-win64.exe`** from the
   [latest release](https://github.com/kirxkirx/sbigdebug_alt/releases/latest) into a folder you can write to.
2. **Disconnect the camera in NINA** (or close NINA, CCDOps and MaxIm). Only one program can own the camera.
3. Double-click the exe and choose **3 (full diagnostic, about 12 minutes)**. Enter alone also selects option 3.
   It prints the plan (frames, time, disk space) and asks before starting. **Cover the telescope or keep the
   dome dark**: the shutter opens for two short frames.
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

`sbigdiag-win64.exe` loads NINA's bundled 64-bit `SBIGUDrv.dll` (or a 64-bit one in
System32). `sbigdiag-win32.exe` loads the 32-bit system DLL, the one CCDOps uses.
Each exe only ever loads a DLL of its own bitness.

## What it reports

- **Which driver DLL was tested**: path, version, SHA-256 and the name of the build if it is a known one
  (e.g. "SBIGUDrv 4.99 Build 8 x64 = the copy NINA bundles", "5.1.1.0 x64"). Copies that were skipped (wrong
  bitness) or failed to load are listed. The 64-bit run warns if it did *not* use NINA's DLL.
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
  - The camera's **own bias measurement** (`CC_READ_OFFSET`), before and after every baseline frame.
  - The RBI preflash parameters.
  - A warning if cooling is on but the cooler gets 0 % power. The STXL switches its cooler off at ≤ 10.25 V input.
- **Camera events**: the SBIG driver's debug log records the camera's own event messages, such as
  "LV TE Off" (input voltage too low). The driver only takes its log settings from
  `HKCU\Software\SBIG\SBIGUDRV\DebugLog`. So sbigdiag writes them there, lets the driver load them,
  and immediately puts the registry back the way it was; it checks again at exit.
  The log goes to `sbigudrv_debug.log` and is summarised at the end.
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
- **Exposure phases** (from `CC_QUERY_COMMAND_STATUS2`). A long pre-exposure phase means the camera is doing an RBI preflash or a slow shutter move.
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
| shutter-open frames (telescope covered) | In the known RBI-preflash bug, light frames were fine while bias/darks were saturated. |
| 2×2 and 3×3 binning | Is there a readout mode that still works? Is the fault tied to clocking or readout? |
| top / bottom 200-row subframes | Is the damage tied to the position on the CCD, or to how long the readout runs? |
| `DCP_VDD_OPTIMIZED = 0` | Is the driver's Vdd (output amplifier supply) switching involved? By default Vdd is lowered only for exposures ≥ 3 s. |
| `START_SKIP_VDD` flag | Keeps Vdd on during a 5 s integration. |
| `DCP_USB_FIFO_ENABLE = 0` | Is the USB FIFO or pipelining data path involved? |
| `DCP_VERT_FLUSH_CONTROL_ENABLE = 0` | Is background vertical flushing of the KAI CCD involved? |
| `DCP_AUTO_AD_GAIN = 0` | Does the driver's A/D gain write before each readout matter? |
| `DCP_PIXEL_PIPELINE_ENABLE = 0` | Is the pixel pipeline involved? |
| `DCP_OVERSCAN_ROWSCOLS = 1` | Are pixels that never held charge also saturated? If yes, the fault is in the amplifier/analog chain/A-D, not charge on the CCD. |
| any `DCP_*` not at its documented default | Does resetting it help? |
| camera option `vddNormallyOff = 1` | Vdd fully off between exposures, which resets the output amplifier before every frame. A possible workaround. Restored afterwards. |
| camera option `overscanRegions = 1` | Camera-side overscan, as above. Restored afterwards. |
| camera customer options → factory (only if preflash or Vdd-off is set) | Is a stored camera option the cause? Restored afterwards. |
| shutter never moves (`SC_LEAVE_SHUTTER`) | Does the shutter motor's current pulse (supply sag) or its timing matter? |
| 2 ms pause per line | Does readout timing matter? |
| re-link (close and reopen the device) | Does a software re-initialisation clear the state? |
| re-link with `DCP_AUTO_AD_GAIN = 0` | Same, but the driver does not touch the A/D gain. |
| 90 s rest | Does the fault decay with idle time (thermal or supply recovery)? |
| final baseline | Confirms everything is back to normal. |

At the end it prints a table of all frames and some heuristic observations. For
example, "binning 2×2 works while the baseline fails", or "the camera's own offset
measurement is at full scale, which points to the analog chain".

Notes:

- Driver control parameters belong to the process, not to the camera. Changing them
  here does not affect NINA, and they vanish when sbigdiag exits.
- The camera "customer options" are stored in the camera. The full test changes
  Vdd-off and overscan for one experiment each, and sets preflash or Vdd-off back to
  the factory value only if one of them was already on. Every change is read back,
  restored and verified straight away.
- Cooling, fan and filter wheel are never touched.
- Ctrl+C or closing the window stops the run at the next driver call, restores everything and closes the camera.
  While camera options are changed (a few seconds per experiment), further Ctrl+C presses are held back
  until they are restored. No new experiment starts after an interrupt.
- If a restore fails, the run **stops** (no further experiment runs with a setting stuck) and says so first in the
  observations; the exit code is then 5.
- **Limits of cleanup:** a crash or hang inside the SBIG driver, a forced kill, or a power cut while a camera
  option is changed can leave that option changed in the camera. sbigdiag writes a restore journal
  (`%APPDATA%\sbigdiag\pending_restore.json`) before every camera-option change, and the next start of sbigdiag
  (any mode) puts the options back. You can also set them by hand in CCDOps: Camera Setup -> Customer Options
  (factory: overscan off, preflash off, Vdd-normally-off off).
- A watchdog warns on the console if a driver call does not return for 60 s. Every call is written to
  `driver_calls.log` *before* it is made, so a crash or hang shows which call it was in. A fatal crash
  (including an access violation inside the SBIG DLL) is also written to `crash.txt`.
- Windows is kept from going to sleep during the run (a suspended PC resets the USB link mid-readout,
  which would look exactly like the camera fault).
- `CC_READOUT_IN_PROGRESS` is never sent: in the shipped SBIG drivers its handler falls through into
  "set temperature regulation" and crashes (or switches the cooler off).

## Command line

```
sbigdiag-win64.exe -full                    full test without the menu
sbigdiag-win64.exe -quick -n 10             10 bias frames
sbigdiag-win64.exe -custom "exp=5,bin=1,dark=1,top=0,height=200" -n 3
sbigdiag-win64.exe -custom "exp=0" -set "VDD_OPTIMIZED=0,USB_FIFO_ENABLE=0" -n 5   (-set: only with -custom/-quick)
sbigdiag-win64.exe -info                    information only
sbigdiag-win64.exe bad1.fits bad2.xisf      analyse existing files (FITS or uncompressed XISF)
  -dll <path>      use a specific SBIGUDrv.dll (e.g. a different driver version)
  -device usb1     choose a USB camera; -device eth -ip a.b.c.d for Ethernet
  -driverlog       verbose SBIG driver debug log (commands + USB), not only camera events
  -noeventlog      do not enable the driver debug log at all
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
