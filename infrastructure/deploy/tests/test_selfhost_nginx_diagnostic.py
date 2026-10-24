"""Closed nginx failure diagnostics; every host probe is mocked and read-only."""
import contextlib
import importlib.util
import io
import json
import math
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
HELPER = HERE/'selfhost-nginx-diagnostic.py'
JOURNAL = ['sudo','-n','journalctl','--unit=nginx','--no-pager','--output=cat','--lines=60']
SERVICE = ['sudo','-n','systemctl','show','nginx','--property=Result','--value']
SOCKETS = ['sudo','-n','ss','-H','-lntp','sport = :80 or sport = :443']
NGINX_PID = ['sudo','-n','systemctl','show','nginx','--property=MainPID','--value']
APACHE_PID = ['sudo','-n','systemctl','show','apache2','--property=MainPID','--value']
NGINX_CONFIG = ['sudo','-n','nginx','-T','-c','/etc/nginx/concord-selfhost-ci.conf']
PROBES = [JOURNAL,SERVICE,SOCKETS,NGINX_PID,APACHE_PID,NGINX_CONFIG]
OPAQUE = 'private-opaque-diagnostic-sentinel'
REASONS = {'PORT_IN_USE','PERMISSION_DENIED','FILE_OPEN_FAILED','UNKNOWN'}
RESULTS = {'success','exit-code','timeout','signal','core-dump','resources','protocol','start-limit-hit','UNKNOWN'}
OWNERS = {'NONE','NGINX_SYSTEMD','NGINX_OTHER','APACHE_SYSTEMD','APACHE_OTHER','DOCKER_PROXY','OTHER','MIXED','UNKNOWN'}
ENDPOINTS = {'HTTP_80','HTTPS_443','CONTROL_8080','MEDIA_3000','OTHER','UNKNOWN'}
CONFIG_SOURCES = {'OWNED_VHOST','INHERITED','AMBIGUOUS','UNKNOWN'}


def socket_row(name='nginx',pid=1234,port=80):
    return f'LISTEN 0 511 0.0.0.0:{port} 0.0.0.0:* users:(("{name}",pid={pid},fd=6))\n'


class NginxDiagnosticTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not HELPER.is_file():
            raise AssertionError('missing nginx diagnostic helper: selfhost-nginx-diagnostic.py')
        spec=importlib.util.spec_from_file_location('tested_selfhost_nginx_diagnostic',HELPER)
        cls.helper=importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.helper)

    def setUp(self):
        self.calls=[]
        self.journal='nginx: [emerg] bind() failed (98: Address already in use)\n'+OPAQUE
        self.selected_port=None
        self.expected_endpoint='UNKNOWN'
        self.expected_config='UNKNOWN'
        self.service='exit-code\n'
        self.journal_error=None; self.service_error=None
        self.journal_rc=0; self.service_rc=0
        self.journal_stderr=OPAQUE; self.service_stderr=OPAQUE
        self.extra={tuple(SOCKETS):[socket_row(),0,None],
                    tuple(NGINX_PID):['1234\n',0,None],
                    tuple(APACHE_PID):['0\n',0,None],tuple(NGINX_CONFIG):['',0,None]}
        self.extra_stderr={}
        probe=patch.object(self.helper.subprocess,'run',side_effect=self.probe)
        probe.start(); self.addCleanup(probe.stop)

    def probe(self, argv, **kwargs):
        self.assertIsInstance(argv,list,'diagnostic attempted a shell command string')
        self.assertIn(argv,self.expected_probes(),'diagnostic attempted an undeclared host command or wrong bind-port selector')
        self.assertIsNot(kwargs.get('shell',False),True,'diagnostic invoked a shell')
        self.assertFalse({'input','stdin','env','executable','cwd','preexec_fn'} & set(kwargs),
                         'diagnostic supplied input or exported process configuration')
        self.assertTrue(set(kwargs).issubset({'timeout','capture_output','text','encoding','errors','stdout','stderr','check','shell'}),
                        'diagnostic supplied undeclared process options')
        timeout=kwargs.get('timeout')
        self.assertIsInstance(timeout,(int,float))
        self.assertNotIsInstance(timeout,bool)
        self.assertTrue(math.isfinite(timeout) and 0 < timeout <= 5,'host diagnostic is not bounded to five seconds')
        self.assertTrue(kwargs.get('capture_output') is True or
            (kwargs.get('stdout')==subprocess.PIPE and kwargs.get('stderr')==subprocess.PIPE),
            'raw host output can escape capture')
        self.calls.append(list(argv))
        key=tuple(SOCKETS) if argv==self.expected_socket() else tuple(argv)
        if key in self.extra:
            output,rc,error=self.extra[key]
            if error is not None: raise error
            return subprocess.CompletedProcess(argv,rc,stdout=output,stderr=self.extra_stderr.get(key,OPAQUE))
        journal=argv==JOURNAL
        error=self.journal_error if journal else self.service_error
        if error is not None: raise error
        return subprocess.CompletedProcess(argv,self.journal_rc if journal else self.service_rc,
            stdout=self.journal if journal else self.service,
            stderr=self.journal_stderr if journal else self.service_stderr)

    def diagnostic(self):
        stdout,stderr=io.StringIO(),io.StringIO()
        with contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):
            result=self.helper.run_diagnostic()
        self.assertEqual(stdout.getvalue(),'','diagnostic function printed host material')
        self.assertEqual(stderr.getvalue(),'','diagnostic function printed host material')
        self.assertIsInstance(result,dict)
        self.assertEqual(set(result),{'nginx_start_reason','service_result','listener_owner','bind_endpoint','bind_config_source'})
        self.assertIn(result['nginx_start_reason'],REASONS)
        self.assertIn(result['service_result'],RESULTS)
        self.assertIn(result['listener_owner'],OWNERS)
        self.assertIn(result['bind_endpoint'],ENDPOINTS)
        self.assertEqual(result['bind_endpoint'],self.expected_endpoint)
        self.assertIn(result['bind_config_source'],CONFIG_SOURCES)
        self.assertEqual(result['bind_config_source'],self.expected_config)
        self.assertNotIn(OPAQUE,json.dumps(result))
        self.assertCountEqual(self.calls,self.expected_probes(),'diagnostic did not perform exactly the six bounded observations')
        # Preserve the earlier independent three-scalar controls after checking
        # the exact new schema and endpoint above.
        return {key:value for key,value in result.items() if key not in ('bind_endpoint','bind_config_source')}

    def expected_socket(self):
        return SOCKETS if self.selected_port is None else SOCKETS[:-1]+[f'sport = :{self.selected_port}']

    def expected_probes(self):
        return [JOURNAL,SERVICE,self.expected_socket(),NGINX_PID,APACHE_PID,NGINX_CONFIG]

    def assertObservation(self,first,second):
        # Existing reason/result controls also require independent valid ownership.
        if 'bind_endpoint' in first:
            second=dict(second,bind_endpoint=self.expected_endpoint,bind_config_source=self.expected_config)
        self.assertEqual(first,dict(second,listener_owner='NGINX_SYSTEMD'))

    def test_standard_bind_permission_and_file_open_messages_emit_only_closed_reason(self):
        cases=(
            ('nginx: [emerg] bind() to [::]:443 failed (98: Address already in use)','PORT_IN_USE'),
            ('nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)','PORT_IN_USE'),
            ('nginx: [emerg] bind() to [::]:80 failed (13: Permission denied)','PERMISSION_DENIED'),
            ('nginx: [emerg] open() "/'+OPAQUE+'" failed (13: Permission denied)','PERMISSION_DENIED'),
            ('nginx: [emerg] open() "/'+OPAQUE+'" failed (2: No such file or directory)','FILE_OPEN_FAILED'))
        for message,expected in cases:
            with self.subTest(reason=expected,message_shape=message.split('failed')[0]):
                self.calls=[]; self.journal=message+'\n'+OPAQUE
                self.selected_port=443 if '[::]:443' in message else 80 if expected=='PORT_IN_USE' else None
                self.expected_endpoint='HTTPS_443' if self.selected_port==443 else 'HTTP_80' if self.selected_port==80 else 'UNKNOWN'
                self.extra[tuple(SOCKETS)]=[socket_row(port=self.selected_port or 80),0,None]
                self.assertObservation(self.diagnostic(),{'nginx_start_reason':expected,'service_result':'exit-code'})

    def test_all_finite_service_results_are_preserved_without_raw_fields(self):
        for value in sorted(RESULTS-{'UNKNOWN'}):
            with self.subTest(service_result=value):
                self.calls=[]; self.service=value+'\n'
                self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':value})

    def test_unknown_journal_retains_independently_observed_service_result(self):
        self.journal=OPAQUE
        self.assertObservation(self.diagnostic(),{'nginx_start_reason':'UNKNOWN','service_result':'exit-code'})

    def test_unknown_service_scalar_retains_independently_observed_reason(self):
        for value in (OPAQUE,'failed','exit-code\n'+OPAQUE,'','success '+OPAQUE):
            with self.subTest(service_shape=value):
                self.calls=[]; self.service=value
                self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'UNKNOWN'})

    def test_nonzero_probe_does_not_use_partial_stdout_or_stderr(self):
        for failed in ('journal','service'):
            with self.subTest(failed_probe=failed):
                self.calls=[]; self.journal_rc=1 if failed=='journal' else 0; self.service_rc=1 if failed=='service' else 0
                expected={'nginx_start_reason':'UNKNOWN' if failed=='journal' else 'PORT_IN_USE',
                          'service_result':'UNKNOWN' if failed=='service' else 'exit-code'}
                self.assertObservation(self.diagnostic(),expected)

    def test_probe_oserror_timeout_and_checked_nonzero_are_unknown_without_leak(self):
        for failed in ('journal','service'):
            command=JOURNAL if failed=='journal' else SERVICE
            for error in (OSError(OPAQUE),PermissionError(OPAQUE),
                          subprocess.TimeoutExpired(command,5,output=OPAQUE,stderr=OPAQUE),
                          subprocess.CalledProcessError(1,command,output=OPAQUE,stderr=OPAQUE)):
                with self.subTest(failed_probe=failed,error_type=type(error).__name__):
                    self.calls=[]; self.journal_error=error if failed=='journal' else None; self.service_error=error if failed=='service' else None
                    expected={'nginx_start_reason':'UNKNOWN' if failed=='journal' else 'PORT_IN_USE',
                              'service_result':'UNKNOWN' if failed=='service' else 'exit-code'}
                    self.assertObservation(self.diagnostic(),expected)

    def test_raw_stderr_is_never_used_to_classify_or_export(self):
        self.journal=OPAQUE; self.service=OPAQUE
        self.journal_stderr='bind() failed (98: Address already in use) '+OPAQUE
        self.service_stderr='exit-code '+OPAQUE
        self.assertObservation(self.diagnostic(),{'nginx_start_reason':'UNKNOWN','service_result':'UNKNOWN'})

    def test_cli_prints_only_closed_json_without_opaque_host_values(self):
        stdout,stderr=io.StringIO(),io.StringIO()
        with patch.object(sys,'argv',[str(HELPER)]),contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):
            result=self.helper.main()
        self.assertEqual(result,0)
        self.assertObservation(json.loads(stdout.getvalue()),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})
        self.assertEqual(stderr.getvalue(),'')
        self.assertNotIn(OPAQUE,stdout.getvalue())
        self.assertCountEqual(self.calls,self.expected_probes())

    def test_cli_unknown_errors_preserve_independent_closed_listener_observation(self):
        self.journal_error=OSError(OPAQUE)
        self.service_error=subprocess.TimeoutExpired(SERVICE,5,output=OPAQUE,stderr=OPAQUE)
        stdout,stderr=io.StringIO(),io.StringIO()
        with patch.object(sys,'argv',[str(HELPER)]),contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):
            result=self.helper.main()
        self.assertEqual(result,0)
        self.assertObservation(json.loads(stdout.getvalue()),{'nginx_start_reason':'UNKNOWN','service_result':'UNKNOWN'})
        self.assertEqual(stderr.getvalue(),'')
        self.assertNotIn(OPAQUE,stdout.getvalue())

    def test_listener_categories_require_actual_named_pid_association(self):
        cases=[
            ('', '1234','0','NONE'),
            (socket_row(),'1234','0','NGINX_SYSTEMD'),
            (socket_row(port=443),'1234','0','NGINX_SYSTEMD'),
            (socket_row(),'5678','0','NGINX_OTHER'),
            (socket_row(),'0','0','NGINX_OTHER'),
            (socket_row('apache2',2345),'1234','2345','APACHE_SYSTEMD'),
            (socket_row('apache2',2345,443),'1234','3456','APACHE_OTHER'),
            (socket_row('apache2',2345),'1234','0','APACHE_OTHER'),
            (socket_row('docker-proxy',3456),'1234','0','DOCKER_PROXY'),
            (socket_row('custom-httpd',4567),'1234','0','OTHER'),
            (socket_row()+socket_row('apache2',2345,443),'1234','2345','MIXED'),
            (socket_row()+socket_row('docker-proxy',3456,443),'1234','0','MIXED'),
            ('LISTEN 0 511 [::]:443 [::]:* users:(("nginx",pid=1235,fd=6),("nginx",pid=1234,fd=6))\n',
             '1234','0','NGINX_SYSTEMD'),
            (socket_row()+socket_row(pid=1235,port=443),'1234','0','NGINX_SYSTEMD')]
        for sockets,nginx_pid,apache_pid,owner in cases:
            with self.subTest(owner=owner,socket_shape=sockets[:6]):
                self.calls=[]
                self.extra[tuple(SOCKETS)]=[sockets,0,None]
                self.extra[tuple(NGINX_PID)]=[nginx_pid+'\n',0,None]
                self.extra[tuple(APACHE_PID)]=[apache_pid+'\n',0,None]
                self.assertEqual(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE',
                    'service_result':'exit-code','listener_owner':owner})

    def test_malformed_ambiguous_or_oversized_socket_observations_are_unknown(self):
        cases=[OPAQUE,'LISTEN 0 511 0.0.0.0:80 0.0.0.0:*\n',
               socket_row(pid=0),socket_row(pid=-1),socket_row(pid='opaque'),
               socket_row()+OPAQUE, socket_row()*20000,
               'LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=1234)) trailing '+OPAQUE]
        for sockets in cases:
            with self.subTest(shape=sockets[:20],size=len(sockets)):
                self.calls=[]; self.extra[tuple(SOCKETS)]=[sockets,0,None]
                self.assertEqual(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE',
                    'service_result':'exit-code','listener_owner':'UNKNOWN'})

    def test_listener_probe_failures_ignore_partial_output_and_preserve_other_scalars(self):
        for command in (SOCKETS,NGINX_PID,APACHE_PID):
            for failure in ('nonzero',OSError(OPAQUE),
                            subprocess.TimeoutExpired(command,5,output=OPAQUE,stderr=OPAQUE)):
                with self.subTest(probe=command[-2:],failure=type(failure).__name__):
                    self.calls=[]
                    self.extra[tuple(SOCKETS)]=[socket_row('apache2',2345) if command==APACHE_PID else socket_row(),0,None]
                    self.extra[tuple(NGINX_PID)]=['1234\n',0,None]
                    self.extra[tuple(APACHE_PID)]=['2345\n',0,None]
                    self.extra[tuple(command)][1:]=[1,None] if failure=='nonzero' else [0,failure]
                    self.assertEqual(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE',
                        'service_result':'exit-code','listener_owner':'UNKNOWN'})

    def test_invalid_relevant_mainpid_cannot_claim_systemd_ownership(self):
        for command,name,pid in ((NGINX_PID,'nginx',1234),(APACHE_PID,'apache2',2345)):
            for value in ('',OPAQUE,'-1','1234\n5678','1234 '+OPAQUE,'1.2','9'*100):
                with self.subTest(unit=name,pid_shape=value[:10]):
                    self.calls=[]
                    self.extra[tuple(SOCKETS)]=[socket_row(name,pid),0,None]
                    self.extra[tuple(command)]=[value,0,None]
                    self.assertEqual(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE',
                        'service_result':'exit-code','listener_owner':'UNKNOWN'})

    def test_unrelated_unit_failure_does_not_discard_known_listener_owner(self):
        self.extra[tuple(APACHE_PID)]=[OPAQUE,1,OSError(OPAQUE)]
        self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_successful_empty_socket_observation_does_not_require_unit_pid_availability(self):
        self.extra[tuple(SOCKETS)]=['',0,None]
        self.extra[tuple(NGINX_PID)]=[OPAQUE,1,None]
        self.extra[tuple(APACHE_PID)]=[OPAQUE,1,None]
        self.assertEqual(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE',
            'service_result':'exit-code','listener_owner':'NONE'})

    def test_cli_listener_output_never_contains_raw_pid_address_or_process_name(self):
        self.extra[tuple(SOCKETS)]=[socket_row(OPAQUE,987654,443),0,None]
        stdout,stderr=io.StringIO(),io.StringIO()
        with patch.object(sys,'argv',[str(HELPER)]),contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):
            result=self.helper.main()
        self.assertEqual(result,0)
        decoded=json.loads(stdout.getvalue())
        self.assertEqual(set(decoded),{'nginx_start_reason','service_result','listener_owner','bind_endpoint','bind_config_source'})
        self.assertEqual(decoded['bind_endpoint'],'UNKNOWN')
        self.assertEqual(decoded['bind_config_source'],'UNKNOWN')
        self.assertIn(decoded['listener_owner'],{'OTHER','UNKNOWN'})
        for private in (OPAQUE,'987654','0.0.0.0','443'):
            self.assertNotIn(private,stdout.getvalue())
        self.assertEqual(stderr.getvalue(),'')
        self.assertCountEqual(self.calls,self.expected_probes())

    def test_actual_valid_bind_port_controls_socket_query_and_closed_endpoint(self):
        cases=((80,'HTTP_80'),(443,'HTTPS_443'),(8080,'CONTROL_8080'),
               (3000,'MEDIA_3000'),(5017,'OTHER'),(1,'OTHER'),(65535,'OTHER'))
        for port,endpoint in cases:
            for address in ('0.0.0.0','127.0.0.1','[::]','[2001:db8::1]'):
                with self.subTest(endpoint=endpoint,address_family='v6' if '[' in address else 'v4'):
                    self.calls=[]; self.selected_port=port; self.expected_endpoint=endpoint
                    self.journal=f'nginx: [emerg] bind() to {address}:{port} failed (98: Address already in use)\n'
                    self.extra[tuple(SOCKETS)]=[socket_row(port=port),0,None]
                    self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_most_recent_recognized_bind_failure_selects_only_its_port(self):
        self.selected_port=8080; self.expected_endpoint='CONTROL_8080'
        self.journal=('nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)\n'
                      'nginx: [emerg] bind() to [::]:8080 failed (98: Address already in use)\n'+OPAQUE)
        self.extra[tuple(SOCKETS)]=[socket_row('docker-proxy',3456,8080),0,None]
        self.assertEqual(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE',
            'service_result':'exit-code','listener_owner':'DOCKER_PROXY'})

    def test_socket_port_mismatch_cannot_authorize_listener_ownership(self):
        self.selected_port=8080; self.expected_endpoint='CONTROL_8080'
        self.journal='nginx: [emerg] bind() to [::]:8080 failed (98: Address already in use)\n'
        for rows in (socket_row(port=80),socket_row(port=8080)+socket_row(port=443)):
            with self.subTest(mixed_rows=rows.count('LISTEN')>1):
                self.calls=[]; self.extra[tuple(SOCKETS)]=[rows,0,None]
                self.assertEqual(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE',
                    'service_result':'exit-code','listener_owner':'UNKNOWN'})

    def test_invalid_bind_endpoint_never_enters_dynamic_socket_argument(self):
        endpoints=('0.0.0.0:080','0.0.0.0:0','0.0.0.0:65536','0.0.0.0:abc',
                   '0.0.0.0:-1','0.0.0.0:8080;'+OPAQUE,'999.0.0.1:8080',
                   '[not-ipv6]:8080','::1:8080',OPAQUE+':8080')
        for endpoint in endpoints:
            with self.subTest(shape=endpoint[:12]):
                self.calls=[]
                self.journal=f'nginx: [emerg] bind() to {endpoint} failed (98: Address already in use)\n'
                self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_failed_or_oversized_journal_cannot_supply_dynamic_selector(self):
        valid='nginx: [emerg] bind() to [::]:8080 failed (98: Address already in use)\n'
        for journal,rc,error in ((valid,1,None),(valid,0,OSError(OPAQUE)),
                                (valid,0,subprocess.TimeoutExpired(JOURNAL,5,output=valid)),
                                (valid+'x'*65537,0,None)):
            with self.subTest(nonzero=bool(rc),error_type=type(error).__name__,oversized=len(journal)>65536):
                self.calls=[]; self.journal=journal; self.journal_rc=rc; self.journal_error=error
                self.assertObservation(self.diagnostic(),{'nginx_start_reason':'UNKNOWN','service_result':'exit-code'})

    def test_cli_selected_bind_endpoint_never_exports_numeric_port_or_address(self):
        self.selected_port=5017; self.expected_endpoint='OTHER'
        self.journal='nginx: [emerg] bind() to 192.0.2.47:5017 failed (98: Address already in use)\n'
        self.extra[tuple(SOCKETS)]=[socket_row('docker-proxy',987654,5017),0,None]
        stdout,stderr=io.StringIO(),io.StringIO()
        with patch.object(sys,'argv',[str(HELPER)]),contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):
            result=self.helper.main()
        self.assertEqual(result,0)
        self.assertEqual(json.loads(stdout.getvalue()),{'nginx_start_reason':'PORT_IN_USE',
            'service_result':'exit-code','listener_owner':'DOCKER_PROXY','bind_endpoint':'OTHER','bind_config_source':'UNKNOWN'})
        for private in ('5017','192.0.2.47','987654','docker-proxy',OPAQUE):
            self.assertNotIn(private,stdout.getvalue())
        self.assertEqual(stderr.getvalue(),'')
        self.assertCountEqual(self.calls,self.expected_probes())

    def test_config_source_is_bound_to_actual_failed_port_and_exact_owned_section(self):
        owned='/etc/nginx/conf.d/concordvoice.conf'
        inherited='/etc/nginx/sites-enabled/default'
        cases=[(80,'HTTP_80',[(owned,'listen 80;')],'OWNED_VHOST'),
               (443,'HTTPS_443',[(owned,'listen [::]:443 ssl;')],'OWNED_VHOST'),
               (8080,'CONTROL_8080',[(inherited,'listen 8080;')],'INHERITED'),
               (5017,'OTHER',[('/etc/nginx/conf.d/custom.conf','listen 0.0.0.0:5017;')],'INHERITED'),
               (80,'HTTP_80',[(owned,'listen 80;'),(inherited,'listen 80;')],'AMBIGUOUS'),
               (8080,'CONTROL_8080',[(owned,'listen 80;'),(inherited,'listen 8080;')],'INHERITED'),
               (80,'HTTP_80',[(owned,'listen 443 ssl;')],'UNKNOWN'),
               (80,'HTTP_80',[(owned+'.bak','listen 80;')],'INHERITED')]
        for port,endpoint,sections,expected in cases:
            with self.subTest(port_class=endpoint,source=expected):
                self.calls=[]; self.selected_port=port; self.expected_endpoint=endpoint; self.expected_config=expected
                self.journal=f'nginx: [emerg] bind() to [::]:{port} failed (98: Address already in use)\n'
                self.extra[tuple(SOCKETS)]=[socket_row(port=port),0,None]
                output=''.join(f'# configuration file {path}:\nserver {{\n  {directive}\n}}\n'
                               for path,directive in sections)
                self.extra[tuple(NGINX_CONFIG)]=[output,0,None]
                self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_missing_bind_endpoint_cannot_classify_even_matching_configuration(self):
        self.extra[tuple(NGINX_CONFIG)]=['# configuration file /etc/nginx/conf.d/concordvoice.conf:\nserver {\nlisten 80;\n}\n',0,None]
        self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_config_failed_malformed_or_oversized_observations_never_export_partial_content(self):
        self.selected_port=8080; self.expected_endpoint='CONTROL_8080'
        self.journal='nginx: [emerg] bind() to [::]:8080 failed (98: Address already in use)\n'
        self.extra[tuple(SOCKETS)]=[socket_row(port=8080),0,None]
        valid='# configuration file /etc/nginx/sites-enabled/default:\nserver {\nlisten 8080;\n}\n'
        cases=[(valid,1,None),(valid,0,OSError(OPAQUE)),
               (valid,0,subprocess.TimeoutExpired(NGINX_CONFIG,5,output=valid,stderr=OPAQUE)),
               (valid,0,subprocess.CalledProcessError(1,NGINX_CONFIG,output=valid,stderr=OPAQUE)),
               (OPAQUE,0,None),('listen 8080;\n',0,None),
               ('# configuration file relative.conf:\nlisten 8080;\n',0,None),
               ('# configuration file /etc/nginx/conf.d/concordvoice.conf:\nlisten opaque;\n',0,None),
               (valid+'x'*65537,0,None)]
        for output,rc,error in cases:
            with self.subTest(nonzero=bool(rc),error_type=type(error).__name__,oversized=len(output)>65536):
                self.calls=[]; self.extra[tuple(NGINX_CONFIG)]=[output,rc,error]
                self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_config_comment_is_not_an_active_listen_directive(self):
        self.selected_port=8080; self.expected_endpoint='CONTROL_8080'
        self.journal='nginx: [emerg] bind() to [::]:8080 failed (98: Address already in use)\n'
        self.extra[tuple(SOCKETS)]=[socket_row(port=8080),0,None]
        self.extra[tuple(NGINX_CONFIG)]=['# configuration file /etc/nginx/conf.d/concordvoice.conf:\nserver {\n# listen 8080;\nlisten 80;\n}\n',0,None]
        self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_config_combined_stdout_stderr_budget_is_bounded(self):
        self.selected_port=8080; self.expected_endpoint='CONTROL_8080'
        self.journal='nginx: [emerg] bind() to [::]:8080 failed (98: Address already in use)\n'
        self.extra[tuple(SOCKETS)]=[socket_row(port=8080),0,None]
        self.extra[tuple(NGINX_CONFIG)]=['# configuration file /etc/nginx/conf.d/concordvoice.conf:\nserver {\nlisten 8080;\n}\n',0,None]
        self.extra_stderr[tuple(NGINX_CONFIG)]=OPAQUE+'x'*65536
        self.assertObservation(self.diagnostic(),{'nginx_start_reason':'PORT_IN_USE','service_result':'exit-code'})

    def test_cli_configuration_paths_and_values_are_never_emitted(self):
        self.selected_port=5017; self.expected_endpoint='OTHER'; self.expected_config='INHERITED'
        self.journal='nginx: [emerg] bind() to [::]:5017 failed (98: Address already in use)\n'
        self.extra[tuple(SOCKETS)]=[socket_row(port=5017),0,None]
        path='/etc/nginx/conf.d/'+OPAQUE+'.conf'
        self.extra[tuple(NGINX_CONFIG)]=[f'# configuration file {path}:\nserver {{\nlisten 5017;\nserver_name {OPAQUE};\n}}\n',0,None]
        stdout,stderr=io.StringIO(),io.StringIO()
        with patch.object(sys,'argv',[str(HELPER)]),contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):
            result=self.helper.main()
        self.assertEqual(result,0)
        self.assertEqual(json.loads(stdout.getvalue()),{'nginx_start_reason':'PORT_IN_USE',
            'service_result':'exit-code','listener_owner':'NGINX_SYSTEMD','bind_endpoint':'OTHER','bind_config_source':'INHERITED'})
        for private in (OPAQUE,path,'5017','1234','server_name','listen 5017;'):
            self.assertNotIn(private,stdout.getvalue())
        self.assertEqual(stderr.getvalue(),'')
        self.assertCountEqual(self.calls,self.expected_probes())


class NginxDiagnosticWorkflowTests(unittest.TestCase):
    def test_failure_only_diagnostic_follows_the_exact_acceptance_step(self):
        import yaml
        workflow=yaml.safe_load((ROOT/'.github/workflows/selfhost-acceptance.yml').read_text())
        steps=workflow['jobs']['fresh-host-selfhost-acceptance']['steps']
        exact=[index for index,step in enumerate(steps) if step.get('id')=='exact-acceptance']
        self.assertEqual(len(exact),1,'exact acceptance lacks its stable step id')
        helper_path='infrastructure/deploy/tests/selfhost-nginx-diagnostic.py'
        diagnostics=[(index,step) for index,step in enumerate(steps) if helper_path in step.get('run','')]
        self.assertEqual(len(diagnostics),1,'failure-only nginx diagnostic step is missing or duplicated')
        index,step=diagnostics[0]
        self.assertGreater(index,exact[0],'nginx diagnosis runs before exact acceptance')
        self.assertEqual(step.get('if'),"failure() && steps.exact-acceptance.outcome == 'failure'",
                         'nginx diagnosis must require failure of the exact acceptance step')
        self.assertEqual(step['run'].strip(),'python3 "$GITHUB_WORKSPACE/'+helper_path+'"',
                         'diagnosis must invoke only the fixed helper from authenticated workflow tooling')
        self.assertNotIn('env',step,'diagnosis must not export workflow environment or credentials')
        self.assertFalse(any('journalctl' in item.get('run','') for item in steps),'workflow contains a raw journal export')


if __name__=='__main__':
    unittest.main()
