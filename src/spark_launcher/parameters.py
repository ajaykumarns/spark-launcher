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
        if count:return changed,count
        pattern=r'''(args\[args\.index\(["']'''+flag+r'''["']\)\s*\+\s*1\]\s*(?:=|==)\s*["'])[0-9]+(?:\.[0-9]+)?(["'])'''
        return re.subn(pattern,lambda m:m.group(1)+value+m.group(2),text)
    return re.subn(pattern,lambda m:m.group(1)+value,text)


def render_parameters(spec):
    parameters=spec.get('parameters',{})
    outputs=[];matched=set()
    for binding in spec.get('bindings',[]):
        path=Path(binding['path'])
        if not path.is_absolute():raise SparkError('Recipe binding path must be absolute.')
        original=path.read_bytes();text=original.decode()
        style=binding['format']
        if style=='nvidia_json':
            document=json.loads(text)
            commands=[document['serve'],*document['serve'].get('additional_commands',[])]
            for field,flag in binding['fields'].items():
                for command in commands:
                    updated,count=replace_flag(command['command'],flag,parameters[field],'command')
                    if count:command['command']=updated;matched.add(field)
            text=json.dumps(document,indent=2)+'\n'
        elif style=='env':
            for field,key in binding['fields'].items():
                value=parameters[field]
                if not isinstance(value,(int,float)) or isinstance(value,bool):raise SparkError('Recipe values must be numeric.')
                text,count=re.subn(r'(?m)^('+re.escape(key)+r'=)[0-9]+(?:\.[0-9]+)?[ \t]*$',lambda m:m.group(1)+str(value),text)
                if count:matched.add(field)
        elif style=='docker_env':
            for field,key in binding['fields'].items():
                value=parameters[field]
                if not isinstance(value,(int,float)) or isinstance(value,bool):raise SparkError('Recipe values must be numeric.')
                text,count=re.subn(r'(\b'+re.escape(key)+r'=)[0-9]+(?:\.[0-9]+)?',lambda m:m.group(1)+str(value),text)
                if count:matched.add(field)
        elif style in ('python','command'):
            for field,flag in binding['fields'].items():
                text,count=replace_flag(text,flag,parameters[field],style)
                if count:matched.add(field)
        else:raise SparkError('Unknown recipe binding format.')
        outputs.append((path,original,text.encode()))
    if parameters and set(parameters)!=matched:
        raise SparkError('Some settings do not match the installed recipe: '+', '.join(sorted(set(parameters)-matched)))
    changed=[]
    for path,original,data in outputs:
        if original!=data:
            backup=path.with_name(path.name+'.before-sparky')
            if not backup.exists():atomic_write(backup,original)
            atomic_write(path,data,stat.S_IMODE(path.stat().st_mode))
            changed.append(str(path))
    return {'changed_files':changed,'parameter_fields':sorted(matched)}
