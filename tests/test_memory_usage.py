import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from spark_launcher.memory_usage import aggregate, classify, context_limit, gpu_processes, host_memory
from spark_launcher.status import format_status
from test_resources import configuration


class MemoryTests(unittest.TestCase):
    def test_gpu_process_and_child_are_attributed_without_adding_host_and_gpu(self):
        rows = aggregate([{'pid': 10, 'owner': 'qwen', 'pss': 400},
                          {'pid': 11, 'owner': 'qwen', 'pss': 600}],
                         {10: 1000, 11: 2000}, True, {'qwen': 'Qwen'})
        self.assertEqual(rows[0]['gpu_bytes'], 3000)
        self.assertEqual(rows[0]['host_pss_bytes'], 1000)
        self.assertNotIn('total_bytes', rows[0])

    def test_cgroup_matching_is_exact_and_nested_not_substring(self):
        sources = {'/system.slice/docker-abc.scope': 'qwen', '/system.slice/docker-abcd.scope': 'other', '/': 'bad'}
        self.assertEqual(classify('/system.slice/docker-abc.scope/child', sources), 'qwen')
        self.assertEqual(classify('/system.slice/docker-abcd.scope', sources), 'other')
        self.assertEqual(classify('/unknown', sources), 'system')

    def test_unknown_gpu_is_not_reported_as_zero(self):
        parsed = gpu_processes('12, 256\n13, [Not Supported]\n')
        self.assertEqual(parsed[12], 256*1024**2)
        self.assertIsNone(parsed[13])
        row = aggregate([{'pid': 12, 'owner': 'qwen', 'pss': 100}], {}, False, {})[0]
        self.assertIsNone(row['gpu_bytes'])

    def test_disappearing_gpu_process_is_not_assigned_to_a_model(self):
        rows = aggregate([], {123: 500}, True, {'system': 'Other host processes'})
        self.assertEqual(rows[0]['id'], 'system')
        self.assertEqual(rows[0]['gpu_bytes'], 500)

    def test_host_reading_uses_pss_instead_of_rss(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path/'status').write_text('VmRSS: 999 kB\n')
            (path/'smaps_rollup').write_text('Rss: 999 kB\nPss: 123 kB\n')
            self.assertEqual(host_memory(path), 123*1024)

    def test_context_uses_actual_placement_and_backend_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            (Path(directory)/'vllm').mkdir()
            (Path(directory)/'vllm/api.env').write_text('VLLM_API_KEY=fixture-only\n')
            config['models']['qwen']['settings']['spark01']['context_tokens'] = 111
            config['models']['qwen']['settings']['spark02']['context_tokens'] = 222
            state = {'nodes': ['spark01']}
            with patch('spark_launcher.memory_usage.request_json', return_value={'data': [{'id': 'qwen', 'max_model_len': 333}]}):
                self.assertEqual(context_limit(config, 'qwen', state), {'context_tokens': 333, 'context_source': 'running backend'})
            with patch('spark_launcher.memory_usage.request_json', return_value={'data': [{'id': 'another-model', 'max_model_len': 444}]}):
                self.assertEqual(context_limit(config, 'qwen', state), {'context_tokens': 111, 'context_source': 'configured'})

    def test_dual_node_model_keeps_separate_memory_readings(self):
        from spark_launcher.memory_usage import enrich
        from spark_launcher.resources import write_cluster
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = configuration(root)
            config['models']['qwen']['run_on'] = 'both'
            write_cluster(root/'config/models.json', config)
            status = {'models': {'qwen': {'kind': 'llm', 'loaded': True, 'state': 'ready',
                                         'nodes': ['spark01', 'spark02']}}, 'comfyui': {}}
            def sample(config, node):
                return {'workloads': [{'id': 'qwen', 'name': 'qwen', 'gpu_bytes': 10 if node == 'spark01' else 20,
                                       'host_pss_bytes': 3}], 'warnings': []}
            with patch('spark_launcher.memory_usage.read_node', side_effect=sample), \
                 patch('spark_launcher.memory_usage.context_limit', return_value={'context_tokens': 262144, 'context_source': 'running backend'}):
                result = enrich(root, status)
            model = result['models']['qwen']
            self.assertEqual(model['memory_by_node']['spark01']['gpu_bytes'], 10)
            self.assertEqual(model['memory_by_node']['spark02']['gpu_bytes'], 20)
            self.assertEqual(model['context_tokens'], 262144)
            self.assertNotIn('memory_total_bytes', model)


    def test_status_shows_context_per_node_memory_and_no_llm_calls_header(self):
        data = {'nodes': {'spark01': {'memory_bytes': {'MemTotal': 128*1024**3, 'MemAvailable': 20*1024**3}}},
                'models': {'qwen': {'kind': 'llm', 'loaded': True, 'state': 'ready', 'nodes': ['spark01']}},
                'memory_usage': {'spark01': {'workloads': [{'id': 'qwen', 'name': 'qwen', 'kind': 'llm',
                    'state': 'ready', 'context_tokens': 262144, 'context_source': 'running backend',
                    'gpu_bytes': 80*1024**3, 'host_pss_bytes': 4*1024**3}]}}}
        output = format_status(data)
        for value in ('262,144', '80.0 GiB', '4.0 GiB', 'GPU RAM', 'HOST RAM', 'spark01'):
            self.assertIn(value, output)
        self.assertNotIn('LLM CALLS', output)
        self.assertIn('Do not add them.', output)

    def test_context_fallback_is_labeled_and_failed_memory_is_unknown(self):
        data = {'nodes': {'spark01': {'memory_bytes': {}}},
                'models': {'qwen': {'kind': 'llm', 'loaded': True, 'state': 'ready', 'nodes': ['spark01'],
                                    'context_tokens': 262144, 'context_source': 'configured'}}}
        output = format_status(data)
        self.assertIn('262,144*', output)
        self.assertIn('unknown', output)
        self.assertIn('running backend did not report', output)


if __name__ == '__main__':
    unittest.main()
