"""Compile numeric settings into the installed, pinned engine recipes."""
from __future__ import annotations
import json
import re
import stat
from pathlib import Path
from .catalog import SparkError, atomic_write


def replace_flag(text,flag,value,style):
    flag=re.escape(flag)
    value=str(value)
    if not re.fullmatch(r'[0-9]+(?:\.[0-9]+)?',value):raise SparkError('Recipe parameters must be numeric.')
    if style=='command':
        pattern=r'('+flag+r'\s+)[0-9]+(?:\.[0-9]+)?(?=\s|$)'
    else:
        # Python argv lists, keyword dictionaries, and explicit argv overrides.
        pattern=r'''(["']'''+flag+r'''["']\s*[, :]\s*["'])[0-9]+(?:\.[0-9]+)?(["'])'''
        changed,count=re.subn(pattern,lambda m:m.group(1)+value+m.group(2),text)
        pattern=r'''(args\[args\.index\(["']'''+flag+r'''["']\)\s*\+\s*1\]\s*(?:=|==)\s*["'])[0-9]+(?:\.[0-9]+)?(["'])'''
        changed,overrides=re.subn(pattern,lambda m:m.group(1)+value+m.group(2),changed)
        return changed,count+overrides
    return re.subn(pattern,lambda m:m.group(1)+value,text)


def render_parameters(spec):
    parameters=spec.get('parameters',{})
    outputs=[];matched=set();paths=set()
    for binding in spec.get('bindings',[]):
        path=Path(binding['path'])
        if not path.is_absolute():raise SparkError('Recipe binding path must be absolute.')
        if path in paths:raise SparkError('A recipe file must have one binding with all its fields.')
        paths.add(path)
        original=path.read_bytes();text=original.decode()
        style=binding['format']
        binding_matched=set()
        if style=='nvidia_json':
            document=json.loads(text)
            commands=[document['serve'],*document['serve'].get('additional_commands',[])]
            for field,flag in binding['fields'].items():
                for command in commands:
                    updated,count=replace_flag(command['command'],flag,parameters[field],'command')
                    if count:command['command']=updated;binding_matched.add(field)
            text=json.dumps(document,indent=2)+'\n'
        elif style=='env':
            for field,key in binding['fields'].items():
                value=parameters[field]
                if not isinstance(value,(int,float)) or isinstance(value,bool):raise SparkError('Recipe values must be numeric.')
                text,count=re.subn(r'(?m)^('+re.escape(key)+r'=)[0-9]+(?:\.[0-9]+)?[ \t]*$',lambda m:m.group(1)+str(value),text)
                if count:binding_matched.add(field)
        elif style=='docker_env':
            for field,key in binding['fields'].items():
                value=parameters[field]
                if not isinstance(value,(int,float)) or isinstance(value,bool):raise SparkError('Recipe values must be numeric.')
                text,count=re.subn(r'(\b'+re.escape(key)+r'=)[0-9]+(?:\.[0-9]+)?',lambda m:m.group(1)+str(value),text)
                if count:binding_matched.add(field)
        elif style in ('python','command'):
            for field,flag in binding['fields'].items():
                text,count=replace_flag(text,flag,parameters[field],style)
                if count:binding_matched.add(field)
        else:raise SparkError('Unknown recipe binding format.')
        missing=set(binding['fields'])-binding_matched
        if missing:
            raise SparkError('Recipe fields did not match '+str(path)+': '+', '.join(sorted(missing)))
        matched.update(binding_matched)
        outputs.append((path,original,text.encode()))
    if parameters and set(parameters)!=matched:
        raise SparkError('Some settings do not match the installed recipe: '+', '.join(sorted(set(parameters)-matched)))
    changed=[]
    written=[]
    try:
        for path,original,data in outputs:
            if original!=data:
                mode=stat.S_IMODE(path.stat().st_mode)
                backup=path.with_name(path.name+'.before-sparky')
                if not backup.exists():atomic_write(backup,original)
                written.append((path,original,mode))
                atomic_write(path,data,mode)
                changed.append(str(path))
    except BaseException as error:
        failures=[]
        for path,original,mode in reversed(written):
            try:atomic_write(path,original,mode)
            except OSError:failures.append(str(path))
        if failures:raise SparkError('Recipe rollback failed for: '+', '.join(failures)) from error
        raise
    return {'changed_files':changed,'parameter_fields':sorted(matched)}
