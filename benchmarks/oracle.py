"""Record real-input counts using the retained Python reference parser."""
import argparse
import hashlib
import json
from pathlib import Path
import sys

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'python'))
import survey_data

p=argparse.ArgumentParser();p.add_argument('--inputs',required=True);p.add_argument('--output',required=True);args=p.parse_args()
files=[]
for path in sorted(Path(args.inputs).glob('*.csv')):
    h=hashlib.sha256(path.read_bytes()).hexdigest()
    valid=invalid=non_wifi=zero=0
    with path.open(encoding='utf-8-sig',newline='') as f:
        reader,line=survey_data.csv_reader(f)
        for i,r in enumerate(reader,line):
            if not survey_data.is_wifi(r):non_wifi+=1;continue
            try:o=survey_data.parse_observation(r,i)
            except ValueError:invalid+=1;continue
            valid+=1
            if o['latitude']==0 and o['longitude']==0:zero+=1
    files.append(dict(name=path.name,bytes=path.stat().st_size,sha256=h,valid_rows=valid,invalid_rows=invalid,non_wifi_rows=non_wifi,zero_coordinate_rows=zero))
out={'schema':1,'oracle':'upstream python/survey_data.py','files':files,'total_valid_rows':sum(f['valid_rows'] for f in files)}
Path(args.output).parent.mkdir(parents=True,exist_ok=True);Path(args.output).write_text(json.dumps(out,indent=2)+'\n',encoding='utf-8')
print(json.dumps({'files':len(files),'expected_rows':out['total_valid_rows'],'zero_rows':sum(f['zero_coordinate_rows'] for f in files)}))
