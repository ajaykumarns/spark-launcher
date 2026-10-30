import sys,tempfile,unittest,zipfile
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'tools'))
from install_node_helper import install

class NodeUpgradeTests(unittest.TestCase):
    def test_upgrade_preserves_old_offsets_and_uses_new_code(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);target=root/'sparky';incoming=root/'incoming.pyz'
            with zipfile.ZipFile(target,'w') as output:output.writestr('spark_launcher/cli.py','old source')
            before=target.read_bytes()
            with zipfile.ZipFile(target) as old:offset=old.start_dir
            with zipfile.ZipFile(incoming,'w') as output:
                output.writestr('spark_launcher/cli.py','new source');output.writestr('spark_launcher/ocr.py','OCR source')
            result=install(incoming,target)
            self.assertEqual(target.read_bytes()[:offset],before[:offset]);self.assertEqual(Path(result['backup']).read_bytes(),before)
            with zipfile.ZipFile(target) as output:self.assertEqual(output.read('spark_launcher/cli.py'),b'new source')
    def test_bad_archive_does_not_replace_installed_helper(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);target=root/'sparky';target.write_bytes(b'keep this helper');incoming=root/'incoming.pyz'
            with zipfile.ZipFile(incoming,'w') as output:output.writestr('unrelated','data')
            with self.assertRaises(ValueError):install(incoming,target)
            self.assertEqual(target.read_bytes(),b'keep this helper')

if __name__=='__main__':unittest.main()
