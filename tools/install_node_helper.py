"""Install a node zipapp while preserving code offsets used by live processes."""
import argparse,hashlib,json,os,shutil,tempfile,time,warnings,zipfile
from pathlib import Path


def install(archive,target):
    archive=Path(archive).expanduser();target=Path(target).expanduser()
    with zipfile.ZipFile(archive) as incoming:
        if incoming.testzip() is not None:raise ValueError('The helper archive is damaged.')
        if 'spark_launcher/cli.py' not in incoming.namelist():raise ValueError('Not a Sparky helper archive.')
    target.parent.mkdir(parents=True,exist_ok=True)
    original=target.read_bytes() if target.exists() else None
    backup=None;prefix=None
    if original is not None:
        folder=target.parent/'.sparky-backups';folder.mkdir(mode=0o700,exist_ok=True)
        backup=folder/('sparky-'+str(time.time_ns())+'.pyz');backup.write_bytes(original);backup.chmod(0o700)
        if zipfile.is_zipfile(target):
            with zipfile.ZipFile(target) as previous:prefix=original[:previous.start_dir]
    fd,name=tempfile.mkstemp(dir=target.parent);os.close(fd);stage=Path(name)
    try:
        if prefix is None:shutil.copyfile(archive,stage)
        else:
            stage.write_bytes(original)
            # Appending retains old local-entry offsets. A running controller's
            # cached zip directory can still import its original modules.
            with warnings.catch_warnings():
                warnings.simplefilter('ignore',UserWarning)
                with zipfile.ZipFile(archive) as incoming,zipfile.ZipFile(stage,'a',compression=zipfile.ZIP_DEFLATED) as output:
                    for entry in incoming.infolist():output.writestr(entry,incoming.read(entry.filename))
            if stage.read_bytes()[:len(prefix)]!=prefix:raise ValueError('Existing helper code offsets changed.')
        with zipfile.ZipFile(stage) as check:
            if check.testzip() is not None:raise ValueError('Installed helper archive is damaged.')
        if original is not None and target.read_bytes()!=original:raise ValueError('Sparky changed during installation. Retry after the other update finishes.')
        stage.chmod(0o755);os.replace(stage,target)
    finally:stage.unlink(missing_ok=True)
    return {'installed':str(target),'backup':str(backup) if backup else None,
            'sha256':hashlib.sha256(target.read_bytes()).hexdigest(),'existing_code_offsets_preserved':prefix is not None}


def main():
    parser=argparse.ArgumentParser();parser.add_argument('--archive',required=True,type=Path)
    parser.add_argument('--target',type=Path,default=Path.home()/'.local/bin/sparky')
    args=parser.parse_args();print(json.dumps(install(args.archive,args.target)))

if __name__=='__main__':main()
