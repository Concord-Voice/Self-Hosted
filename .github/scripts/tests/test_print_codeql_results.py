"""Offline CodeQL metadata boundary; generated SARIF never reaches a host sink."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[3]
HELPER = ROOT / '.github/scripts/print-codeql-results.py'
CANARY = 'private-opaque-codeql-canary'
RULE = 'js/path-injection'
SOURCE = 'services/media-plane/src/server.ts'

# Execute the real CLI with filesystem writes, child processes, and sockets
# denied. The parser may read only; -B also prevents incidental bytecode writes.
LAUNCHER = r'''
import os,runpy,sys
finding_source=os.path.abspath('services/media-plane/src/server.ts')
def audit(event,args):
    if event=='open':
        if isinstance(args[0],(str,bytes,os.PathLike)) and os.path.abspath(os.fsdecode(args[0]))==finding_source:
            raise RuntimeError('forbidden finding-source read')
        mode=args[1]; flags=args[2]
        if (isinstance(mode,str) and any(char in mode for char in 'wax+')) or (isinstance(flags,int) and flags&(os.O_WRONLY|os.O_RDWR|os.O_CREAT|os.O_TRUNC|os.O_APPEND)):
            raise RuntimeError('forbidden write')
    if event.startswith(('socket.','subprocess.')) or event in ('os.system','os.posix_spawn','os.exec','os.remove','os.rename','os.mkdir','os.rmdir','os.chmod','os.symlink','os.link'):
        raise RuntimeError('forbidden side effect')
sys.addaudithook(audit)
sys.argv=sys.argv[1:]
runpy.run_path(sys.argv[0],run_name='__main__')
'''


def finding():
    return {'ruleId':RULE,'ruleIndex':0,'level':'warning','message':{'text':CANARY},
            'locations':[{'physicalLocation':{'artifactLocation':{'uri':SOURCE,'uriBaseId':'%SRCROOT%'},
                                             'region':{'startLine':37,'snippet':{'text':CANARY}}}}],
            'codeFlows':[{'threadFlows':[{'locations':[{'location':{'message':{'text':CANARY}}}]}]}]}


def sarif(results=None):
    return {'version':'2.1.0','$schema':'https://json.schemastore.org/sarif-2.1.0.json',
            'runs':[{'tool':{'driver':{'name':'CodeQL','rules':[{'id':RULE,
                     'properties':{'security-severity':'8.1'},'help':{'text':CANARY}}]}},
                     'originalUriBaseIds':{'%SRCROOT%':{'uri':'file:///private/'+CANARY+'/'}},
                     'artifacts':[{'location':{'uri':SOURCE},'contents':{'text':CANARY}}],
                     'results':[finding()] if results is None else results}]}


class CodeQLResultMetadataTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not HELPER.is_file():
            raise AssertionError('missing closed CodeQL metadata helper: print-codeql-results.py')
        spec=importlib.util.spec_from_file_location('tested_codeql_metadata',HELPER)
        cls.helper=importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.helper)

    def setUp(self):
        self.assertTrue(HELPER.is_file(),'missing closed CodeQL metadata helper: print-codeql-results.py')
        temporary=tempfile.TemporaryDirectory(prefix='owned-codeql-sarif-')
        self.addCleanup(temporary.cleanup)
        self.work=Path(temporary.name)
        self.directory=self.work/'generated-sarif'; self.directory.mkdir()
        self.report=self.directory/'javascript.sarif'
        self.marker=self.work/'outside-marker'; self.marker.write_text(CANARY)
        self.finding_source=self.work/SOURCE
        self.finding_source.parent.mkdir(parents=True)
        self.finding_source.write_text(CANARY)

    def write(self,payload):
        self.report.write_text(json.dumps(payload),encoding='utf-8')

    def invoke(self,args=None):
        before={path:path.read_bytes() for path in self.work.rglob('*') if path.is_file() and not path.is_symlink()}
        result=subprocess.run([sys.executable,'-I','-B','-c',LAUNCHER,str(HELPER),
                               *([str(self.directory)] if args is None else args)],
                              cwd=self.work,env={'PATH':'','PYTHONDONTWRITEBYTECODE':'1'},
                              text=True,capture_output=True,check=False,timeout=5)
        after={path:path.read_bytes() for path in self.work.rglob('*') if path.is_file() and not path.is_symlink()}
        self.assertEqual(after,before,'metadata reporting modified owned inputs or marker')
        self.assertNotIn(CANARY,result.stdout+result.stderr,'raw SARIF or input identity escaped')
        self.assertNotIn(str(self.work),result.stdout+result.stderr,'absolute host path escaped')
        return result

    def assert_refused(self,result):
        self.assertNotEqual(result.returncode,0,'unsafe SARIF admitted')
        self.assertEqual(result.stdout,'','refusal emitted partial result metadata')
        self.assertLessEqual(len(result.stderr),200,'refusal exported an unbounded exception')
        self.assertNotIn('Traceback',result.stderr)

    def test_valid_result_outputs_only_closed_metadata_and_never_raw_sarif_fields(self):
        self.write(sarif())
        self.assertEqual(self.helper.load_results(self.directory),
                         [{'rule_id':RULE,'path':SOURCE,'line':37,'level':'warning','security_severity':8.1}])
        result=self.invoke()
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(result.stderr,'')
        self.assertEqual([json.loads(line) for line in result.stdout.splitlines()],
                         [{'rule_id':RULE,'path':SOURCE,'line':37,'level':'warning','security_severity':8.1}])

    def test_character_offset_regions_include_zero_without_fabricating_a_line(self):
        for offset,length in ((0,None),(0,0),(0,9),(12,None),(12,0),(12,9)):
            with self.subTest(offset=offset,length=length):
                region={'charOffset':offset,'snippet':{'text':CANARY}}
                if length is not None: region['charLength']=length
                payload=sarif()
                payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']=region
                self.write(payload); result=self.invoke()
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertEqual(result.stderr,'')
                self.assertEqual(json.loads(result.stdout),
                                 {'rule_id':RULE,'path':SOURCE,'level':'warning','security_severity':8.1})

    def test_line_regions_retain_positive_line_with_valid_character_coordinates(self):
        for region in ({'startLine':1},{'startLine':37,'charOffset':0},
                       {'startLine':37,'charOffset':12,'charLength':0},
                       {'startLine':37,'charLength':9}):
            with self.subTest(region=region):
                payload=sarif()
                payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']=region
                self.write(payload); result=self.invoke()
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertEqual(json.loads(result.stdout),
                                 {'rule_id':RULE,'path':SOURCE,'line':region['startLine'],
                                  'level':'warning','security_severity':8.1})

    def test_malformed_character_offsets_are_refused_even_with_a_valid_line(self):
        for offset in (True,False,-1,1.5,'0',None,{},[]):
            for with_line in (False,True):
                with self.subTest(offset_type=type(offset).__name__,with_line=with_line):
                    region={'charOffset':offset}
                    if with_line: region['startLine']=37
                    payload=sarif()
                    payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']=region
                    self.write(payload); self.assert_refused(self.invoke())

    def test_malformed_character_lengths_are_refused_even_with_a_valid_line(self):
        for length in (True,False,-1,1.5,'0',None,{},[]):
            for with_line in (False,True):
                with self.subTest(length_type=type(length).__name__,with_line=with_line):
                    region={'charOffset':0,'charLength':length}
                    if with_line: region['startLine']=37
                    payload=sarif()
                    payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']=region
                    self.write(payload); self.assert_refused(self.invoke())

    def test_malformed_line_does_not_fall_back_to_valid_character_offset(self):
        for line in (0,-1,True,False,1.5,'37',None):
            with self.subTest(line_type=type(line).__name__):
                payload=sarif()
                payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']={
                    'startLine':line,'charOffset':0}
                self.write(payload); self.assert_refused(self.invoke())

    def test_absent_start_coordinates_and_length_only_regions_are_refused(self):
        for region in ({},{'snippet':{'text':CANARY}},{'charLength':0},{'charLength':9}):
            with self.subTest(region_keys=list(region)):
                payload=sarif()
                payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']=region
                self.write(payload); self.assert_refused(self.invoke())

    def test_missing_sarif_output_refuses_but_valid_empty_results_succeed(self):
        self.assert_refused(self.invoke())
        self.write(sarif([]))
        result=self.invoke()
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(result.stdout+result.stderr,'')

    def test_multiple_files_runs_and_results_are_reported_without_extra_fields(self):
        payload=sarif(); payload['runs'].append(copy.deepcopy(payload['runs'][0]))
        self.write(payload)
        (self.directory/'go.sarif').write_text(json.dumps(sarif()))
        result=self.invoke()
        self.assertEqual(result.returncode,0,result.stderr)
        rows=[json.loads(line) for line in result.stdout.splitlines()]
        self.assertEqual(len(rows),3)
        self.assertTrue(all(row=={'rule_id':RULE,'path':SOURCE,'line':37,'level':'warning','security_severity':8.1} for row in rows))

    def test_ruleid_only_without_driver_rules_is_valid_without_optional_severity(self):
        payload=sarif(); del payload['runs'][0]['tool']['driver']['rules']
        del payload['runs'][0]['results'][0]['ruleIndex']
        self.write(payload); result=self.invoke()
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(json.loads(result.stdout),{'rule_id':RULE,'path':SOURCE,'line':37,'level':'warning'})

    def test_ruleindex_only_resolves_rule_identity_and_normalizes_primary_uri(self):
        payload=sarif(); del payload['runs'][0]['results'][0]['ruleId']
        payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['artifactLocation']['uri']='%73ervices/media-plane/src/server.ts'
        self.write(payload); result=self.invoke()
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(json.loads(result.stdout),{'rule_id':RULE,'path':SOURCE,'line':37,'level':'warning','security_severity':8.1})

    def test_invalid_optional_level_or_security_severity_is_omitted(self):
        for level,severity in ((CANARY,CANARY),('warning','NaN'),('note',True),('error','10.1'),('none','-1')):
            with self.subTest(level=level,severity_shape=type(severity).__name__):
                payload=sarif(); payload['runs'][0]['results'][0]['level']=level
                payload['runs'][0]['tool']['driver']['rules'][0]['properties']['security-severity']=severity
                self.write(payload); result=self.invoke()
                self.assertEqual(result.returncode,0,result.stderr)
                expected={'rule_id':RULE,'path':SOURCE,'line':37}
                if level in {'none','note','warning','error'}: expected['level']=level
                self.assertEqual(json.loads(result.stdout),expected)

    def test_malformed_json_or_sarif_is_closed_without_canary(self):
        invalid=[None,[],{}, {'version':'2.0.0','runs':[]}, {'version':'2.1.0','runs':CANARY},
                 {'version':'2.1.0','runs':[{}]}]
        for payload in invalid:
            with self.subTest(envelope_type=type(payload).__name__):
                self.write(payload); self.assert_refused(self.invoke())
        self.report.write_text('{"'+CANARY+'":')
        with self.assertRaises(ValueError):
            self.helper.load_results(self.directory)
        self.assert_refused(self.invoke())

    def test_absolute_traversal_scheme_and_opaque_primary_paths_are_refused(self):
        paths=['/'+CANARY+'/file.ts','../'+CANARY+'.ts','src/../'+CANARY+'.ts',
               'file:///private/'+CANARY,'https://example.invalid/'+CANARY,
               'C:\\'+CANARY+'\\file.ts','src\\file.ts','src//file.ts',
               'src/%2e%2e/'+CANARY+'.ts','%2f'+CANARY+'.ts','src/file.ts?'+CANARY,
               'src/file.ts#'+CANARY,'src/\n'+CANARY+'.ts','']
        for uri in paths:
            with self.subTest(path_kind=paths.index(uri)):
                payload=sarif(); payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['artifactLocation']['uri']=uri
                self.write(payload); self.assert_refused(self.invoke())

    def test_missing_opaque_location_and_nonpositive_noninteger_line_are_refused(self):
        for line in (0,-1,True,1.5,'37',None):
            with self.subTest(line_type=type(line).__name__):
                payload=sarif(); payload['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']['startLine']=line
                self.write(payload); self.assert_refused(self.invoke())
        for locations in ([],[{'logicalLocations':[{'name':CANARY}]}],[CANARY]):
            payload=sarif(); payload['runs'][0]['results'][0]['locations']=locations
            self.write(payload); self.assert_refused(self.invoke())

    def test_query_identifiers_and_mismatched_rule_association_are_refused(self):
        for rule in (CANARY+'\n','../'+CANARY,'js/'+CANARY+'?opaque','js/'+CANARY+' '+CANARY,'',None):
            payload=sarif(); payload['runs'][0]['results'][0]['ruleId']=rule
            self.write(payload); self.assert_refused(self.invoke())
        for index in (True,-1,1,'0'):
            payload=sarif(); payload['runs'][0]['results'][0]['ruleIndex']=index
            self.write(payload); self.assert_refused(self.invoke())
        payload=sarif(); payload['runs'][0]['results'][0]['ruleId']='js/xss'
        self.write(payload); self.assert_refused(self.invoke())

    def test_result_and_file_bounds_refuse_atomically(self):
        self.write(sarif([finding() for _ in range(1001)]))
        self.assert_refused(self.invoke())
        oversized=json.dumps(sarif()).encode()+b' '*(10*1024*1024)
        self.report.write_bytes(oversized)
        self.assert_refused(self.invoke())
        valid=sarif(); invalid=copy.deepcopy(valid)
        invalid['runs'][0]['results'][0]['locations'][0]['physicalLocation']['region']['startLine']=0
        self.write(valid); (self.directory/'z-invalid.sarif').write_text(json.dumps(invalid))
        self.assert_refused(self.invoke())

    def test_file_count_and_aggregate_result_bounds_are_closed(self):
        for index in range(100):
            (self.directory/f'report-{index:03d}.sarif').write_text(json.dumps(sarif([])))
        result=self.invoke(); self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(result.stdout+result.stderr,'')
        (self.directory/'report-100.sarif').write_text(json.dumps(sarif([])))
        self.assert_refused(self.invoke())
        for path in self.directory.iterdir(): path.unlink()
        self.write(sarif([finding() for _ in range(500)]))
        (self.directory/'second.sarif').write_text(json.dumps(sarif([finding() for _ in range(500)])))
        result=self.invoke(); self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(len(result.stdout.splitlines()),1000)
        self.write(sarif([finding() for _ in range(501)]))
        (self.directory/'second.sarif').write_text(json.dumps(sarif([finding() for _ in range(500)])))
        self.assert_refused(self.invoke())

    def test_symlink_directory_or_file_and_missing_directory_are_refused(self):
        self.write(sarif())
        link=self.work/'linked-directory'; link.symlink_to(self.directory,target_is_directory=True)
        self.assert_refused(self.invoke([str(link)]))
        actual=self.work/'outside.sarif'; actual.write_text(json.dumps(sarif()))
        self.report.unlink(); self.report.symlink_to(actual)
        self.assert_refused(self.invoke())
        self.assert_refused(self.invoke([str(self.work/('missing-'+CANARY))]))

    def test_cli_bad_arity_refuses_without_exporting_arguments(self):
        for args in ([],[str(self.directory),CANARY]):
            self.assert_refused(self.invoke(args))


if __name__=='__main__':
    unittest.main()
