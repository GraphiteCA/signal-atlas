#pragma OPENCL EXTENSION cl_khr_fp64 : enable
#pragma OPENCL FP_CONTRACT OFF
__kernel void scores(__global const double *x, __global const double *y,
 __global const double *r, __global const double *w, __global const int *g,
 __global const double *sw, __global const double *px, __global const double *py,
 __global double *imp, __global double *means, __global double *out,
 int n, int k, int base, double floor_m, double factor, double within) {
 int local_p=get_global_id(0), p=base+local_p;
 for(int q=0;q<k;q++) means[local_p*k+q]=0;
 for(int j=0;j<n;j++) {
  double dx=px[p]-x[j],dy=py[p]-y[j];
  double d=sqrt(dx*dx+dy*dy);if(d<floor_m)d=floor_m;
  double v=r[j]+factor*log10(d);
  imp[local_p*n+j]=v;means[local_p*k+g[j]]+=v*w[j];
 }
 for(int q=0;q<k;q++)means[local_p*k+q]/=sw[q];
 double total=0;
 for(int j=0;j<n;j++) {double e=imp[local_p*n+j]-means[local_p*k+g[j]];total+=e*e*w[j];}
 out[p]=total+within;
}
