//go:build windows && amd64

package gpu

import (
	"bytes"
	_ "embed"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed scoring.cl
var source []byte

type clDevice struct {
	lib                                     *windows.DLL
	procs                                   map[string]*windows.Proc
	context, queue, program, kernel, device uintptr
	gate                                    chan struct{}
	buffers                                 [11]uintptr
	capacity                                [11]int
	failedPins                              []*runtime.Pinner
	grid                                    *CandidateGrid
	failed                                  bool
}

func (d *clDevice) invoke(name string, args ...any) uintptr {
	// Keep Go arguments tracked and pinned until the synchronous driver call
	// returns. OpenCL copies scalar kernel arguments and blocking transfers.
	var pin runtime.Pinner
	defer pin.Unpin()
	values := make([]uintptr, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case uintptr:
			values[i] = v
		case int:
			values[i] = uintptr(v)
		case unsafe.Pointer:
			if v != nil {
				pin.Pin(v)
				values[i] = uintptr(v)
			}
		default:
			panic(fmt.Sprintf("unsupported OpenCL ABI argument %T", arg))
		}
	}
	v, _, _ := d.procs[name].Call(values...)
	runtime.KeepAlive(args)
	return v
}
func (d *clDevice) call(name string, args ...any) (uintptr, error) {
	v := d.invoke(name, args...)
	if int32(v) != 0 {
		return v, fmt.Errorf("%s: OpenCL %d", name, int32(v))
	}
	return v, nil
}
func ptr[T any](v *T) unsafe.Pointer { return unsafe.Pointer(v) }
func cbytes(v []byte) unsafe.Pointer {
	if len(v) == 0 {
		return nil
	}
	return ptr(&v[0])
}
func clError(name string, e int32) error {
	if e != 0 {
		return fmt.Errorf("%s: OpenCL %d", name, e)
	}
	return nil
}
func open() (out device, name string, err error) {
	d := &clDevice{procs: map[string]*windows.Proc{}, gate: make(chan struct{}, 1)}
	defer func() {
		if err != nil {
			d.close()
		}
	}()
	// Never resolve a DLL from a survey directory or the current directory.
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, "", err
	}
	d.lib, err = windows.LoadDLL(filepath.Join(system, "OpenCL.dll"))
	if err != nil {
		return nil, "", err
	}
	for _, n := range []string{"clGetPlatformIDs", "clGetDeviceIDs", "clGetDeviceInfo", "clCreateContext", "clCreateCommandQueue", "clCreateProgramWithSource", "clBuildProgram", "clGetProgramBuildInfo", "clCreateKernel", "clCreateBuffer", "clSetKernelArg", "clEnqueueWriteBuffer", "clFinish", "clEnqueueReadBuffer", "clEnqueueNDRangeKernel", "clReleaseMemObject", "clReleaseKernel", "clReleaseProgram", "clReleaseCommandQueue", "clReleaseContext"} {
		d.procs[n], err = d.lib.FindProc(n)
		if err != nil {
			return nil, "", err
		}
	}
	var platforms [16]uintptr
	var count uint32
	if _, err = d.call("clGetPlatformIDs", 16, ptr(&platforms[0]), ptr(&count)); err != nil {
		return nil, "", err
	}
	for _, platform := range platforms[:min(count, 16)] {
		var devices [16]uintptr
		var n uint32
		if _, e := d.call("clGetDeviceIDs", platform, 4, 16, ptr(&devices[0]), ptr(&n)); e != nil {
			continue
		}
		for _, dev := range devices[:min(n, 16)] {
			var fp uint64
			if _, e := d.call("clGetDeviceInfo", dev, 0x1032, 8, ptr(&fp), 0); e == nil && fp != 0 {
				d.device = dev
				break
			}
		}
		if d.device != 0 {
			break
		}
	}
	if d.device == 0 {
		return nil, "", fmt.Errorf("No float64-capable OpenCL GPU found; CPU fallback is active")
	}
	buf := make([]byte, 4096)
	if _, err = d.call("clGetDeviceInfo", d.device, 0x102B, uintptr(len(buf)), cbytes(buf), 0); err != nil {
		return nil, "", err
	}
	name = string(bytes.TrimRight(buf, "\x00"))
	var code int32
	d.context = d.invoke("clCreateContext", 0, 1, ptr(&d.device), 0, 0, ptr(&code))
	if err = clError("clCreateContext", code); err != nil {
		return nil, name, err
	}
	d.queue = d.invoke("clCreateCommandQueue", d.context, d.device, 0, ptr(&code))
	if err = clError("clCreateCommandQueue", code); err != nil {
		return nil, name, err
	}
	text := append(append([]byte{}, source...), 0)
	textPtr := cbytes(text)
	d.program = d.invoke("clCreateProgramWithSource", d.context, 1, ptr(&textPtr), 0, ptr(&code))
	runtime.KeepAlive(text)
	if err = clError("clCreateProgramWithSource", code); err != nil {
		return nil, name, err
	}
	options := append([]byte("-cl-std=CL1.2"), 0)
	if _, err = d.call("clBuildProgram", d.program, 1, ptr(&d.device), cbytes(options), 0, 0); err != nil {
		log := make([]byte, 16384)
		d.call("clGetProgramBuildInfo", d.program, d.device, 0x1183, uintptr(len(log)), cbytes(log), 0)
		return nil, name, fmt.Errorf("%w: %s", err, strings.TrimRight(string(log), "\x00"))
	}
	entry := append([]byte("scores"), 0)
	d.kernel = d.invoke("clCreateKernel", d.program, cbytes(entry), ptr(&code))
	if err = clError("clCreateKernel", code); err != nil {
		return nil, name, err
	}
	return d, name, nil
}
func (d *clDevice) close() {
	if d.lib == nil {
		return
	}
	for _, v := range d.buffers {
		if v != 0 {
			d.call("clReleaseMemObject", v)
		}
	}
	for _, v := range []struct {
		n string
		v uintptr
	}{{"clReleaseKernel", d.kernel}, {"clReleaseProgram", d.program}, {"clReleaseCommandQueue", d.queue}, {"clReleaseContext", d.context}} {
		if v.v != 0 {
			d.call(v.n, v.v)
		}
	}
	d.lib.Release()
	d.lib = nil
}
func (d *clDevice) buffer(i, size int) error {
	if d.capacity[i] >= size {
		return nil
	}
	if d.buffers[i] != 0 {
		d.call("clReleaseMemObject", d.buffers[i])
		d.buffers[i] = 0
		d.capacity[i] = 0
	}
	var code int32
	v := d.invoke("clCreateBuffer", d.context, 1, uintptr(size), 0, ptr(&code))
	if e := clError("clCreateBuffer", code); e != nil {
		return e
	}
	d.buffers[i], d.capacity[i] = v, size
	_, e := d.call("clSetKernelArg", d.kernel, uintptr(i), unsafe.Sizeof(v), ptr(&v))
	return e
}
func (d *clDevice) write(i int, p unsafe.Pointer, size int) error {
	_, e := d.call("clEnqueueWriteBuffer", d.queue, d.buffers[i], 0, 0, uintptr(size), p, 0, 0, 0)
	return e
}
func (d *clDevice) score(in Input, cancelled func() bool) (out []float64, err error) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if cancelled != nil && cancelled() {
			return nil, ErrCancelled
		}
		select {
		case d.gate <- struct{}{}:
			goto acquired
		case <-tick.C:
		}
	}
acquired:
	defer func() { <-d.gate }()
	if d.failed {
		return nil, fmt.Errorf("GPU queue could not be synchronized; CPU fallback is active")
	}
	// Non-blocking writes retain pinned Go storage until the ordered queue
	// finishes. Drain on every error path before releasing the pins or gate.
	var transfers runtime.Pinner
	finished := false
	defer func() {
		if !finished {
			if _, e := d.call("clFinish", d.queue); e != nil {
				// A failed driver fence must never expose movable/freed Go
				// storage to a possibly outstanding DMA. Disable this queue
				// and retain this bounded lease for the process lifetime.
				d.failed = true
				d.failedPins = append(d.failedPins, &transfers)
				runtime.KeepAlive(in)
				return
			}
		}
		transfers.Unpin()
		runtime.KeepAlive(in)
	}()
	n, k, p := len(in.X), len(in.WeightSums), len(in.CandidateX)
	// Short dispatches let the display GPU schedule map rendering between tiles.
	tile := min(p, (64<<20)/(8*(n+k)), max(1, (4<<20)/n))
	if max(d.capacity[8], tile*n*8)+max(d.capacity[9], tile*k*8) > 64<<20 {
		for _, i := range []int{8, 9} {
			if d.buffers[i] != 0 {
				d.call("clReleaseMemObject", d.buffers[i])
				d.buffers[i], d.capacity[i] = 0, 0
			}
		}
	}
	cachedGrid := in.Grid != nil && in.Grid == d.grid
	if !cachedGrid {
		d.grid = nil
	}
	arrays := [][]float64{in.X, in.Y, in.RSSI, in.Weights, nil, in.WeightSums, in.CandidateX, in.CandidateY}
	for i, a := range arrays {
		if cachedGrid && i >= 6 {
			continue
		}
		size := len(a) * 8
		if i == 4 {
			size = n * 4
		}
		if e := d.buffer(i, size); e != nil {
			return nil, e
		}
		var e error
		if i == 4 {
			transfers.Pin(&in.Groups[0])
			e = d.write(i, ptr(&in.Groups[0]), size)
		} else {
			transfers.Pin(&a[0])
			e = d.write(i, ptr(&a[0]), size)
		}
		if e != nil {
			return nil, e
		}
	}
	d.grid = in.Grid
	runtime.KeepAlive(in)
	for i, size := range []int{tile * n * 8, tile * k * 8, p * 8} {
		if e := d.buffer(i+8, size); e != nil {
			return nil, e
		}
	}
	for i, v := range []int32{int32(n), int32(k)} {
		if _, e := d.call("clSetKernelArg", d.kernel, uintptr(i+11), 4, ptr(&v)); e != nil {
			return nil, e
		}
	}
	for i, v := range []float64{in.Floor, in.Factor, in.Within} {
		if _, e := d.call("clSetKernelArg", d.kernel, uintptr(i+14), 8, ptr(&v)); e != nil {
			return nil, e
		}
	}
	for offset := 0; offset < p; offset += tile {
		base := int32(offset)
		global := uintptr(min(tile, p-offset))
		if _, e := d.call("clSetKernelArg", d.kernel, 13, 4, ptr(&base)); e != nil {
			return nil, e
		}
		if _, e := d.call("clEnqueueNDRangeKernel", d.queue, d.kernel, 1, 0, ptr(&global), 0, 0, 0, 0); e != nil {
			return nil, e
		}
	}
	out = make([]float64, p)
	transfers.Pin(&out[0])
	if _, e := d.call("clEnqueueReadBuffer", d.queue, d.buffers[10], 1, 0, uintptr(p*8), ptr(&out[0]), 0, 0, 0); e != nil {
		return nil, e
	}
	finished = true
	runtime.KeepAlive(out)
	if cancelled != nil && cancelled() {
		return nil, ErrCancelled
	}
	for _, v := range out {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("GPU returned invalid candidate scores")
		}
	}
	return out, nil
}
