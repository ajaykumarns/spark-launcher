"""Small OpenAI image adapter for tested, local ComfyUI workflows."""
from __future__ import annotations
import base64
import secrets
import time
from urllib.parse import urlencode
from urllib.request import urlopen
from .catalog import SparkError,request_json
from .resources import resolve_workload,selected


def image_models(config):
    return [{'id':name,'object':'model','owned_by':'garuda','input_modalities':['text'],
             'output_modalities':['image'],'supported_endpoints':['/v1/images/generations']}
            for name,model in config['models'].items() if model.get('image_api',{}).get('enabled')]


def flux_workflow(prompt,width,height,seed):
    def node(kind,**inputs):return {'class_type':kind,'inputs':inputs}
    return {
        '1':node('UNETLoader',unet_name='flux-2-klein-4b-fp8.safetensors',weight_dtype='default'),
        '2':node('CLIPLoader',clip_name='qwen_3_4b.safetensors',type='flux2',device='default'),
        '3':node('VAELoader',vae_name='flux2-vae.safetensors'),
        '4':node('CLIPTextEncode',clip=['2',0],text=prompt),
        '5':node('ConditioningZeroOut',conditioning=['4',0]),
        '6':node('CFGGuider',model=['1',0],positive=['4',0],negative=['5',0],cfg=1.0),
        '7':node('RandomNoise',noise_seed=seed),
        '8':node('KSamplerSelect',sampler_name='euler'),
        '9':node('Flux2Scheduler',steps=4,width=width,height=height),
        '10':node('EmptyFlux2LatentImage',width=width,height=height,batch_size=1),
        '11':node('SamplerCustomAdvanced',noise=['7',0],guider=['6',0],sampler=['8',0],sigmas=['9',0],latent_image=['10',0]),
        '12':node('VAEDecode',samples=['11',0],vae=['3',0]),
        '13':node('SaveImage',images=['12',0],filename_prefix='Sparky-FLUX2-Klein')}


def generate(coordinator,data):
    coordinator.refresh()
    name,model=resolve_workload(coordinator.config,data.get('model'))
    api=model.get('image_api',{})
    if name!='flux2-klein-4b' or not api.get('enabled'):
        raise ValueError('This image model has no tested gateway workflow. Use flux2-klein-4b.')
    unknown=set(data)-{'model','prompt','size','n','response_format','seed','stream','quality','user'}
    if unknown:raise ValueError('Unsupported image fields: '+', '.join(sorted(unknown)))
    prompt=data.get('prompt')
    if not isinstance(prompt,str) or not prompt.strip() or len(prompt)>8000:raise ValueError('Give an image prompt of 1 to 8000 characters.')
    if type(data.get('n',1)) is not int or data.get('n',1)!=1:raise ValueError('Generate one image per request.')
    if data.get('stream',False) is not False:raise ValueError('Image streaming is not supported.')
    if data.get('quality','auto') not in ('auto','standard'):raise ValueError('Use auto or standard image quality.')
    if data.get('response_format','b64_json')!='b64_json':raise ValueError('Use response_format b64_json.')
    size=data.get('size','1024x1024')
    if size not in ('512x512','768x768','1024x1024'):raise ValueError('Use 512x512, 768x768, or 1024x1024.')
    width,height=map(int,size.split('x'));seed=data.get('seed',secrets.randbits(63))
    if type(seed) is not int or not 0<=seed<2**64:raise ValueError('Image seed must be an unsigned 64-bit integer.')
    on=api.get('node',model['run_on']);selected(coordinator.config,name,on)
    # One gateway image job across the cluster. Preserve active work on timeout.
    if not coordinator.image_gate.acquire(timeout=600):raise SparkError('The image queue is busy. Retry later.')
    try:
        started=time.monotonic();submitted=coordinator.submit_comfy(flux_workflow(prompt,width,height,seed),on)
        prompt_id=submitted['prompt_id'];url=coordinator.config['nodes'][on]['comfy_url']
        deadline=time.monotonic()+900
        while time.monotonic()<deadline:
            history=request_json(url+'/history/'+prompt_id,timeout=10).get(prompt_id)
            if history:
                status=history.get('status',{})
                if status.get('status_str')=='error':raise SparkError('The image workflow failed. Check the ComfyUI job history.')
                if status.get('completed'):
                    images=history.get('outputs',{}).get('13',{}).get('images',[])
                    if not images:raise SparkError('The image workflow returned no saved image.')
                    item=images[0]
                    query=urlencode({k:item[k] for k in ('filename','subfolder','type')})
                    with urlopen(url+'/view?'+query,timeout=30) as response:png=response.read(16*1024*1024+1)
                    if not png.startswith(b'\x89PNG\r\n\x1a\n') or len(png)>16*1024*1024:raise SparkError('Invalid image output.')
                    coordinator.runtime.event('image_api_completed',workload=name,node=on,prompt_id=prompt_id,
                                              seconds=time.monotonic()-started,width=width,height=height,seed=seed)
                    return {'created':int(time.time()),'data':[{'b64_json':base64.b64encode(png).decode()}]}
            time.sleep(1)
        raise SparkError('The image request timed out. The accepted ComfyUI job was kept; check its queue before retrying.')
    finally:coordinator.image_gate.release()
