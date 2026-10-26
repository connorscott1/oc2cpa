#!/usr/bin/env python3
"""Check native length guards before any payload pointer can be dereferenced."""
import ctypes as c
import json
from pathlib import Path
root=Path(__file__).resolve().parents[1]
manifests=list((root/'dist').rglob('*.build.json'))
assert len(manifests)==1
manifest=json.loads(manifests[0].read_text())
library=c.CDLL(str(manifests[0].parent/manifest['library']))
class Buffer(c.Structure): _fields_=[('ptr',c.c_void_p),('len',c.c_size_t)]
library.cliproxyPluginCall.argtypes=[c.c_char_p,c.c_void_p,c.c_size_t,c.POINTER(Buffer)]
library.cliproxyPluginCall.restype=c.c_int
library.cliproxyPluginFree.argtypes=[c.c_void_p,c.c_size_t]
for pointer,length in ((None,1),(1,129*1024*1024),(1,2**31+1)):
    response=Buffer()
    rc=library.cliproxyPluginCall(b'auth.identifier',pointer,length,c.byref(response))
    try:
        data=json.loads(c.string_at(response.ptr,response.len))
        assert rc!=0 and data['ok'] is False and data['error']['http_status']==413
    finally:
        library.cliproxyPluginFree(response.ptr,response.len)
response=Buffer()
rc=library.cliproxyPluginCall(b'auth.identifier',None,0,c.byref(response))
try:
    data=json.loads(c.string_at(response.ptr,response.len))
    assert rc==0 and data['result']['identifier']=='oc2cpa'
finally:
    library.cliproxyPluginFree(response.ptr,response.len)
print('Native null/oversized/integer-overflow guards and valid ABI dispatch passed.')
