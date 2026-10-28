#!/usr/bin/env python3
"""Prepare public update metadata. Never exports a signing key or diagnostics."""
import argparse, pathlib, shutil, hashlib, json, subprocess, re, os
p=argparse.ArgumentParser();p.add_argument('apk',type=pathlib.Path);p.add_argument('output',type=pathlib.Path);a=p.parse_args()
sdk=pathlib.Path(os.environ['ANDROID_HOME'])/'build-tools/36.0.0'
badging=subprocess.check_output([str(sdk/'aapt'),'dump','badging',str(a.apk)],text=True)
m=re.search(r"package: name='([^']+)' versionCode='(\d+)' versionName='([^']+)'",badging);assert m
subprocess.run([str(sdk/'apksigner'),'verify',str(a.apk)],check=True)
assert m[1]=='com.argus.connect.alpha.debug'
a.output.mkdir(parents=True,exist_ok=True);dest=a.output/'Argus-Connect-alpha.apk';shutil.copyfile(a.apk,dest)
sha=hashlib.sha256(dest.read_bytes()).hexdigest();data={'applicationId':m[1],'versionCode':int(m[2]),'versionName':m[3],'minSdk':int(re.search(r"sdkVersion:'(\d+)'",badging)[1]),'asset':dest.name,'sha256':sha}
(a.output/'update.json').write_text(json.dumps(data,indent=2)+'\n');(a.output/'SHA256.txt').write_text(sha+'  '+dest.name+'\n');print(json.dumps(data))
