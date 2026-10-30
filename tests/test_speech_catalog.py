import tempfile
import unittest
from pathlib import Path

import test_resources as resources
from spark_launcher.catalog import SparkError, make_catalog, resolve, validate_config
from spark_launcher.presentation import format_models, format_config
from spark_launcher.resources import write_cluster
from spark_launcher.coordinator import Coordinator


class SpeechCatalogTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.path = self.root / 'models.json'
        self.c = resources.configuration(self.root)
        self.c['models']['kokoro'] = {
            'kind': 'audio', 'run_on': 'spark01',
            'settings': {'spark01': {'containers': ['kokoro']}},
            'tts': {'model': 'kokoro/kokoro', 'api_base': 'https://spark.example:8443/v1',
                    'default_voice': 'af_bella', 'voices': ['af_bella', 'af_heart'],
                    'formats': ['mp3', 'opus'], 'device': 'cpu', 'api_key_required': False,
                    'api_key': 'private-do-not-publish', 'backend_url': 'http://127.0.0.1:8096'},
        }
        write_cluster(self.path, self.c)
        self.coord = Coordinator(self.path, resources.FakeRuntime(self.c))

    def test_speech_is_discoverable_without_becoming_a_language_model(self):
        wire = self.coord.catalog()
        service = wire['audio_services'][0]
        self.assertEqual(service['default_voice'], 'af_bella')
        self.assertNotIn('api_key', service)
        self.assertNotIn('backend_url', service)
        self.assertFalse(any(row['id'].endswith('/kokoro') for row in wire['data']))
        client = validate_config({'version': 1, 'gateway': 'https://spark.example:8443'})
        catalog = make_catalog(client, wire)
        self.assertEqual(catalog['audio_services'][0]['model'], 'kokoro/kokoro')
        with self.assertRaises(SparkError):
            resolve(catalog, 'kokoro')
        text = format_models(catalog)
        self.assertIn('af_bella', text)
        self.assertIn('Speech generation', text)
        self.assertIn('not required', text)

    def test_audio_details_are_visible_in_config_show(self):
        text = format_config({'path': str(self.path), 'model': 'kokoro', 'config': self.c['models']['kokoro']})
        self.assertIn('af_bella', text)
        self.assertIn('https://spark.example:8443/v1', text)
        self.assertNotIn('private-do-not-publish', text)

    def test_invalid_speech_metadata_is_rejected(self):
        wire = self.coord.catalog()
        wire['audio_services'][0]['voices'] = 'af_bella'
        client = validate_config({'version': 1, 'gateway': 'https://spark.example:8443'})
        with self.assertRaises(SparkError):
            make_catalog(client, wire)
