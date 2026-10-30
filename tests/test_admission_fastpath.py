import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from spark_launcher.catalog import SparkError
from spark_launcher.coordinator import Coordinator
from spark_launcher.resources import write_cluster
from test_resources import configuration,FakeRuntime


class ReadyAdmissionTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name);self.config=configuration(self.root)
        self.path=self.root/'models.json';write_cluster(self.path,self.config)
        self.runtime=FakeRuntime(self.config);self.c=Coordinator(self.path,self.runtime)
        self.ledger=self.root/'controller/running-qwen.json';self.ledger.parent.mkdir()
        self.ledger.write_text(json.dumps({'nodes':['spark02'],'units':{'spark02':'qwen.service'}}))

    def test_ready_model_serves_when_remote_inventory_is_unavailable(self):
        with patch.object(self.runtime,'snapshot',side_effect=SparkError('SSH inventory unavailable')):
            self.c.prepare_llm('qwen')
        self.assertEqual(self.runtime.actions,[('/running',{})])

    def test_new_unit_on_same_node_requires_full_startup_path(self):
        self.config['models']['qwen']['settings']['spark02']['units']['spark02']='qwen-new.service'
        write_cluster(self.path,self.config);self.c.refresh()
        self.runtime.data['models']['qwen'].update(loaded=False,nodes=[],state='stopped')
        with patch.object(self.runtime,'snapshot',wraps=self.runtime.snapshot) as inventory:
            self.c.prepare_llm('qwen')
        inventory.assert_called()
        self.assertEqual(self.runtime.actions,[('/upstream/qwen/health',{'expect_json':False})])

    def test_changed_placement_still_protects_other_loaded_model(self):
        self.config['models']['qwen']['run_on']='both'
        self.config['models']['nemotron']['keep_loaded']=True
        write_cluster(self.path,self.config);self.c.refresh()
        with self.assertRaisesRegex(SparkError,'nemotron must stay loaded'):
            self.c.prepare_llm('qwen')
        self.assertEqual(self.runtime.actions,[])

    def test_starting_manager_state_waits_for_health(self):
        self.runtime.data['models']['qwen']['state']='starting'
        self.c.prepare_llm('qwen')
        self.assertEqual(self.runtime.actions,[('/running',{}),('/upstream/qwen/health',{'expect_json':False})])

    def test_invalid_ledger_keeps_existing_inventory_recovery(self):
        self.ledger.write_text('{broken')
        with patch.object(self.runtime,'snapshot',wraps=self.runtime.snapshot) as inventory:
            self.c.prepare_llm('qwen')
        inventory.assert_called_with(True)


if __name__=='__main__':unittest.main()
