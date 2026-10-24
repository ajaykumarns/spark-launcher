import json
import tempfile
import unittest
from pathlib import Path
from spark_launcher.catalog import SparkError
from spark_launcher.parameters import render_parameters


class ParameterTests(unittest.TestCase):
    def test_one_config_compiles_python_recipe_and_worker_override(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);recipe=root/'recipe.json';worker=root/'worker.py'
            recipe.write_text(json.dumps({'serve':{'command':'export X=1\nvllm serve /model --max-model-len 123 --max-num-seqs 8 --gpu-memory-utilization 0.8'}}))
            worker.write_text('args[args.index("--gpu-memory-utilization") + 1] = "0.75"\nargs[args.index("--max-num-seqs") + 1] = "4"\nassert args[args.index("--max-model-len") + 1] == "123"\n')
            fields={'context_tokens':'--max-model-len','sequences':'--max-num-seqs','gpu':'--gpu-memory-utilization'}
            spec={'parameters':{'context_tokens':456,'sequences':2,'gpu':0.7},'bindings':[{'path':str(recipe),'format':'nvidia_json','fields':fields},{'path':str(worker),'format':'python','fields':fields}]}
            result=render_parameters(spec)
            self.assertEqual(len(result['changed_files']),2)
            self.assertIn('--max-model-len 456',json.loads(recipe.read_text())['serve']['command'])
            self.assertIn('== "456"',worker.read_text());self.assertIn('= "0.7"',worker.read_text())
            self.assertTrue(worker.with_name('worker.py.before-sparky').exists())
    def test_environment_and_shell_render_do_not_merge_lines(self):
        with tempfile.TemporaryDirectory() as temp:
            path=Path(temp)/'values.conf';path.write_text('max_model_len=123\nmax_num_seqs=1\nuntouched=42\n')
            render_parameters({'parameters':{'context':456,'sequences':4},'bindings':[{'path':str(path),'format':'env','fields':{'context':'max_model_len','sequences':'max_num_seqs'}}]})
            self.assertEqual(path.read_text(),'max_model_len=456\nmax_num_seqs=4\nuntouched=42\n')
    def test_missing_field_does_not_partially_write_recipe(self):
        with tempfile.TemporaryDirectory() as temp:
            path=Path(temp)/'recipe.sh';path.write_text('--context 123\n')
            with self.assertRaises(SparkError):render_parameters({'parameters':{'context':456,'missing':4},'bindings':[{'path':str(path),'format':'command','fields':{'context':'--context','missing':'--parallel'}}]})
            self.assertEqual(path.read_text(),'--context 123\n')
