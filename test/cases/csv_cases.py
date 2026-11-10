import csv, io, json, sys
cases = ['a,b\r\nc,d', 'a,b\n\nc', '\ufeffx,"y""z",w\r\n', 'a,"b\r\nc",d\n', 'a,"b"c,d\n', 'a,b"c,d\r', 'a,\n', 'a,', '"unterminated,x\nmore', '\r\n\r\nx', 'a\rb\nc\r\nd', '"a""",b', ',,\n,', 'x,"",y', '"ab"\r', 'a,"q"x"y",z']
out=[]
for c in cases:
    out.append(list(csv.reader(io.StringIO(c.lstrip('\ufeff'), newline=''))))
print(json.dumps({'cases':cases,'out':out}))
