"""Hardware OpenCL scoring pilot; production binaries do not load this runtime.

Uses canonical float64 Go scorer inputs/results, measures upload + dispatch +
readback, and records float64/float32 differences. No DB or fitting cache writes.
"""
import argparse
import ctypes as C
import json
import math
import os
from pathlib import Path
import time

SOURCE=r'''
#pragma OPENCL FP_CONTRACT OFF
TYPEDEF
__kernel void scores(__global const real *x, __global const real *y,
 __global const real *r, __global const real *w, __global const int *g,
 __global const real *sw, __global const real *px, __global const real *py,
 __global real *imp, __global real *means, __global real *out,
 int n, int k, int base) {
 int local_p=get_global_id(0), p=base+local_p;
 for(int q=0;q<k;q++) means[local_p*k+q]=0;
 for(int j=0;j<n;j++) {
  real dx=px[p]-x[j],dy=py[p]-y[j];
  real d=sqrt(dx*dx+dy*dy);if(d<(real)3)d=(real)3;
  real v=r[j]+(real)30*log10(d);
  imp[local_p*n+j]=v;means[local_p*k+g[j]]+=v*w[j];
 }
 for(int q=0;q<k;q++)means[local_p*k+q]/=sw[q];
 real total=0;
 for(int j=0;j<n;j++) {real e=imp[local_p*n+j]-means[local_p*k+g[j]];total+=e*e*w[j];}
 out[p]=total;
}
'''


class OpenCL:
    def __init__(self):
        if os.name!='nt':raise RuntimeError('This pilot uses the Windows OpenCL loader')
        self.dll=C.WinDLL('OpenCL.dll',winmode=0x800)
        self.objects=[];self.context=None;self.queue=None
        P=C.c_void_p;I=C.c_int32;U=C.c_uint32;S=C.c_size_t;Q=C.c_uint64
        signatures={
          'clGetPlatformIDs':(I,[U,P,P]),'clGetDeviceIDs':(I,[P,Q,U,P,P]),
          'clGetDeviceInfo':(I,[P,U,S,P,P]),'clCreateContext':(P,[P,U,P,P,P,P]),
          'clCreateCommandQueue':(P,[P,P,Q,P]),'clCreateProgramWithSource':(P,[P,U,P,P,P]),
          'clBuildProgram':(I,[P,U,P,P,P,P]),'clGetProgramBuildInfo':(I,[P,P,U,S,P,P]),
          'clCreateKernel':(P,[P,P,P]),'clCreateBuffer':(P,[P,Q,S,P,P]),
          'clSetKernelArg':(I,[P,U,S,P]),'clEnqueueWriteBuffer':(I,[P,P,U,S,S,P,U,P,P]),
          'clEnqueueReadBuffer':(I,[P,P,U,S,S,P,U,P,P]),'clEnqueueNDRangeKernel':(I,[P,P,U,P,P,P,U,P,P]),
          'clReleaseMemObject':(I,[P]),'clReleaseKernel':(I,[P]),'clReleaseProgram':(I,[P]),
          'clReleaseCommandQueue':(I,[P]),'clReleaseContext':(I,[P])}
        for name,(restype,args)in signatures.items():
            fn=getattr(self.dll,name);fn.restype=restype;fn.argtypes=args;setattr(self,name,fn)
        platforms=(P*16)();count=U();self.ok(self.clGetPlatformIDs(16,platforms,C.byref(count)))
        self.device=None
        for platform in platforms[:count.value]:
            devices=(P*16)();n=U()
            if self.clGetDeviceIDs(platform,4,16,devices,C.byref(n))==0 and n.value:
                self.device=P(devices[0]);break
        if self.device is None:raise RuntimeError('No hardware OpenCL GPU found')
        error=I();self.context=self.clCreateContext(None,1,C.byref(self.device),None,None,C.byref(error));self.ok(error.value)
        self.queue=self.clCreateCommandQueue(self.context,self.device,0,C.byref(error));self.ok(error.value)

    @staticmethod
    def ok(code):
        if code:raise RuntimeError('OpenCL error '+str(code))

    def info(self,key):
        buf=C.create_string_buffer(16384);n=C.c_size_t()
        self.ok(self.clGetDeviceInfo(self.device,key,len(buf),buf,C.byref(n)))
        return buf.raw[:n.value].rstrip(b'\0').decode()

    def close(self):
        for kind,handle in reversed(self.objects):getattr(self,'clRelease'+kind)(handle)
        self.objects=[]
        if self.queue:self.clReleaseCommandQueue(self.queue);self.queue=None
        if self.context:self.clReleaseContext(self.context);self.context=None

    def run(self,fixture,precision):
        self.objects=[]
        real=C.c_double if precision=='float64'else C.c_float
        code=SOURCE.replace('TYPEDEF','#pragma OPENCL EXTENSION cl_khr_fp64 : enable\ntypedef double real;'if precision=='float64'else 'typedef float real;').encode()
        source=C.c_char_p(code);error=C.c_int32()
        t=time.perf_counter()
        program=self.clCreateProgramWithSource(self.context,1,C.byref(source),None,C.byref(error));self.ok(error.value)
        self.objects.append(('Program',program))
        status=self.clBuildProgram(program,1,C.byref(self.device),b'-cl-std=CL1.2',None,None)
        if status:
            log=C.create_string_buffer(65536);self.clGetProgramBuildInfo(program,self.device,0x1183,len(log),log,None)
            raise RuntimeError(log.value.decode(errors='replace'))
        kernel=self.clCreateKernel(program,b'scores',C.byref(error));self.ok(error.value);self.objects.append(('Kernel',kernel))
        compile_seconds=time.perf_counter()-t
        n=len(fixture['x']);k=len(fixture['weight_sums']);p=len(fixture['scores'])
        # Bound scratch to 64 MiB, even for large full-observation fits.
        tile=min(p,(64*1024**2)//(C.sizeof(real)*(n+k)))
        if tile<1:raise ValueError('Target exceeds bounded GPU scratch')
        buffers=[];host=[]
        for name,kind in [('x',real),('y',real),('rssi',real),('weights',real),('groups',C.c_int32),('weight_sums',real),('candidate_x',real),('candidate_y',real)]:
            values=(kind*len(fixture[name]))(*fixture[name]);host.append(values)
            mem=self.clCreateBuffer(self.context,1,C.sizeof(values),None,C.byref(error));self.ok(error.value)
            self.objects.append(('MemObject',mem));buffers.append(C.c_void_p(mem))
        for length in [tile*n,tile*k,p]:
            mem=self.clCreateBuffer(self.context,1,length*C.sizeof(real),None,C.byref(error));self.ok(error.value)
            self.objects.append(('MemObject',mem));buffers.append(C.c_void_p(mem))
        for i,mem in enumerate(buffers):self.ok(self.clSetKernelArg(kernel,i,C.sizeof(mem),C.byref(mem)))
        for i,value in [(11,n),(12,k)]:
            v=C.c_int32(value);self.ok(self.clSetKernelArg(kernel,i,C.sizeof(v),C.byref(v)))
        out=(real*p)()
        # Candidate grid is shared/immutable; observation data is uploaded each call.
        for i in [6,7]:self.ok(self.clEnqueueWriteBuffer(self.queue,buffers[i],1,0,C.sizeof(host[i]),host[i],0,None,None))
        def call():
            for i in range(6):self.ok(self.clEnqueueWriteBuffer(self.queue,buffers[i],1,0,C.sizeof(host[i]),host[i],0,None,None))
            for offset in range(0,p,tile):
                base=C.c_int32(offset);size=C.c_size_t(min(tile,p-offset))
                self.ok(self.clSetKernelArg(kernel,13,C.sizeof(base),C.byref(base)))
                self.ok(self.clEnqueueNDRangeKernel(self.queue,kernel,1,None,C.byref(size),None,0,None,None))
            self.ok(self.clEnqueueReadBuffer(self.queue,buffers[-1],1,0,C.sizeof(out),out,0,None,None))
        call();call()
        values=[]
        for _ in range(30):
            t=time.perf_counter();call();values.append(time.perf_counter()-t)
        expected=fixture['scores'];actual=list(out)
        if not all(math.isfinite(v)and v>=0 for v in actual):raise RuntimeError('Invalid GPU scores')
        absmax=max(abs(x-y)for x,y in zip(expected,actual))
        relmax=max(abs(x-y)/max(1,abs(x))for x,y in zip(expected,actual))
        oldbest=min(range(p),key=lambda i:expected[i]);newbest=min(range(p),key=lambda i:actual[i])
        values.sort();median=values[len(values)//2]
        result={'name':fixture['name'],'precision':precision,'observations':n,'groups':k,'candidates':p,'tile_candidates':tile,'scratch_bytes':tile*(n+k)*C.sizeof(real),'compile_seconds':compile_seconds,'seconds_p50':median,'seconds_p95':values[math.ceil(len(values)*.95)-1],'cpu_seconds':fixture['cpu_seconds_per_call'],'kernel_stage_speedup':fixture['cpu_seconds_per_call']/median,'score_max_abs_error':absmax,'score_max_relative_error':relmax,'best_coarse_shift_m':math.hypot(fixture['candidate_x'][oldbest]-fixture['candidate_x'][newbest],fixture['candidate_y'][oldbest]-fixture['candidate_y'][newbest]),'samples_seconds':values}
        for kind,handle in reversed(self.objects):getattr(self,'clRelease'+kind)(handle)
        self.objects=[]
        return result


def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--root',type=Path,required=True);args=parser.parse_args()
    root=args.root.resolve(strict=True)
    if json.loads((root/'owner.json').read_text())!={'purpose':'signal-atlas-benchmark','schema':1}:raise ValueError('Owned fixture required')
    output=root/'gpu-report.json'
    if output.exists():raise ValueError('Fresh GPU result required')
    gpu=OpenCL()
    try:
        report={'schema':1,'device':gpu.info(0x102B),'vendor':gpu.info(0x102C),'driver':gpu.info(0x102D),'version':gpu.info(0x102F),'notes':'Screening experiment only; same scoring objective, no fast-math/FMA. Includes per-target input upload and score readback; excludes one-time program compilation. This is not whole-solver or application throughput.','results':[]}
        for file in sorted(root.glob('gpu-input-*.json')):
            fixture=json.loads(file.read_text())
            for precision in ['float64','float32']:
                v=gpu.run(fixture,precision);report['results'].append(v);print(json.dumps(v),flush=True)
        if not report['results']:raise ValueError('Go candidate fixtures required')
        output.write_text(json.dumps(report,indent=2)+'\n')
    finally:gpu.close()


if __name__=='__main__':main()
