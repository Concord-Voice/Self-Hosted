#!/usr/bin/env python3
"""Read bounded local nginx observations and print only closed classifications."""
import json
import ipaddress
import re
import subprocess


_JOURNAL = ['sudo', '-n', 'journalctl', '--unit=nginx', '--no-pager', '--output=cat', '--lines=60']
_SERVICE = ['sudo', '-n', 'systemctl', 'show', 'nginx', '--property=Result', '--value']
_SOCKETS = ['sudo', '-n', 'ss', '-H', '-lntp', 'sport = :80 or sport = :443']
_NGINX_PID = ['sudo', '-n', 'systemctl', 'show', 'nginx', '--property=MainPID', '--value']
_APACHE_PID = ['sudo', '-n', 'systemctl', 'show', 'apache2', '--property=MainPID', '--value']
_CONFIG = ['sudo', '-n', 'nginx', '-T', '-c', '/etc/nginx/concord-selfhost-ci.conf']
_RESULTS = {'success', 'exit-code', 'timeout', 'signal', 'core-dump',
            'resources', 'protocol', 'start-limit-hit'}


def _observe(argv):
    try:
        result = subprocess.run(argv, timeout=5, capture_output=True, text=True,
                                encoding='utf-8', errors='replace', check=False)
        if (type(result.returncode) is not int or result.returncode != 0
                or not isinstance(result.stdout, str) or not isinstance(result.stderr, str)
                or len(result.stdout.encode('utf-8')) + len(result.stderr.encode('utf-8')) > 65536):
            return None
        return result.stdout
    except (OSError, subprocess.SubprocessError):
        return None


def _pid(value, allow_zero=False):
    if value is None or not re.fullmatch(r'(?:0|[1-9][0-9]{0,9})', value.strip()):
        return None
    number = int(value.strip())
    return number if (0 if allow_zero else 1) <= number <= 2147483647 else None


def _endpoint(value, local, selected_port=None):
    match = re.fullmatch(r'(\[[0-9A-Fa-f:.]+\]|[0-9.]+|\*):([1-9][0-9]{0,4})' if local
                         else r'(\[[0-9A-Fa-f:.]+\]|[0-9.]+|\*):\*', value)
    if match is None:
        return False
    if local and int(match[2]) not in ({selected_port} if selected_port is not None else {80, 443}):
        return False
    address = match[1]
    if address == '*':
        return True
    try:
        ipaddress.ip_address(address.strip('[]'))
        return True
    except ValueError:
        return False


def _listener_owner(sockets, nginx_pid, apache_pid, selected_port=None):
    if sockets is None:
        return 'UNKNOWN'
    owners = {}
    for line in sockets.splitlines():
        row = re.fullmatch(r'LISTEN\s+[0-9]+\s+[0-9]+\s+(\S+)\s+(\S+)\s+users:\((.+)\)', line)
        if row is None or not _endpoint(row[1], True, selected_port) or not _endpoint(row[2], False):
            return 'UNKNOWN'
        entries = row[3]
        entry = r'\("([A-Za-z0-9_.-]{1,64})",pid=([1-9][0-9]{0,9}),fd=([0-9]{1,10})\)'
        if re.fullmatch(entry + r'(?:,' + entry + r')*', entries) is None:
            return 'UNKNOWN'
        for name, pid, fd in re.findall(entry, entries):
            number = _pid(pid)
            if number is None or int(fd) > 2147483647:
                return 'UNKNOWN'
            owners.setdefault(name, set()).add(number)
    categories = set()
    for name, pids in owners.items():
        if name in {'nginx', 'apache2'}:
            main_pid = _pid(nginx_pid if name == 'nginx' else apache_pid, allow_zero=True)
            if main_pid is None:
                return 'UNKNOWN'
            prefix = 'NGINX' if name == 'nginx' else 'APACHE'
            categories.add(prefix + ('_SYSTEMD' if main_pid in pids else '_OTHER'))
        else:
            categories.add('DOCKER_PROXY' if name == 'docker-proxy' else 'OTHER')
    if not categories:
        return 'NONE'
    return next(iter(categories)) if len(categories) == 1 else 'MIXED'


def _listen_port(endpoint):
    if re.fullmatch(r'[1-9][0-9]{0,4}', endpoint):
        port = int(endpoint)
        return port if port <= 65535 else None
    match = re.fullmatch(r'(\[[0-9A-Fa-f:.]+\]|[0-9.]+):([1-9][0-9]{0,4})', endpoint)
    if match is None:
        return None
    port = int(match[2])
    if port > 65535 or not _endpoint(endpoint, True, port):
        return None
    return port


def _config_source(config, port):
    if config is None or port is None:
        return 'UNKNOWN'
    section = None
    sources = set()
    for line in config.splitlines():
        stripped = line.strip()
        if stripped.startswith('# configuration file '):
            match = re.fullmatch(r'# configuration file (/[^\s:]+):', stripped)
            if match is None or any(part in {'.', '..', ''} for part in match[1].split('/')[1:]):
                return 'UNKNOWN'
            section = match[1]
        elif not stripped or stripped.startswith('#'):
            continue
        else:
            if section is None:
                return 'UNKNOWN'
            active = stripped.split('#', 1)[0].strip()
            if re.match(r'listen\b', active):
                match = re.fullmatch(r'listen\s+(\S+)(?:\s+(?:ssl|http2|default_server|reuseport|proxy_protocol|ipv6only=(?:on|off)))*\s*;', active)
                if match is None:
                    return 'UNKNOWN'
                actual_port = _listen_port(match[1])
                if actual_port is None:
                    return 'UNKNOWN'
                if actual_port == port:
                    sources.add('OWNED_VHOST' if section == '/etc/nginx/conf.d/concordvoice.conf' else 'INHERITED')
    return 'AMBIGUOUS' if len(sources) > 1 else next(iter(sources)) if sources else 'UNKNOWN'


def run_diagnostic():
    journal = _observe(_JOURNAL)
    service = _observe(_SERVICE)
    reason = 'UNKNOWN'
    port = None
    if journal is not None:
        # The most recent recognized cause wins. Host text is never returned,
        # logged or copied into acceptance evidence.
        for line in reversed(journal.splitlines()):
            if re.search(r'bind\(\).*failed.*Address already in use', line):
                reason = 'PORT_IN_USE'
                match = re.search(r'bind\(\) to (\S+) failed \(98: Address already in use\)', line)
                if match is not None and ':' in match[1]:
                    port = _listen_port(match[1])
            elif re.search(r'(?:bind|open)\(\).*failed.*Permission denied', line):
                reason = 'PERMISSION_DENIED'
            elif re.search(r'open\(\).*failed', line):
                reason = 'FILE_OPEN_FAILED'
            if reason != 'UNKNOWN':
                break
    socket_probe = _SOCKETS if port is None else _SOCKETS[:-1] + [f'sport = :{port}']
    sockets = _observe(socket_probe)
    nginx_pid = _observe(_NGINX_PID)
    apache_pid = _observe(_APACHE_PID)
    config = _observe(_CONFIG)
    value = service.strip() if service is not None else ''
    return {'nginx_start_reason': reason,
            'service_result': value if value in _RESULTS else 'UNKNOWN',
            'listener_owner': _listener_owner(sockets, nginx_pid, apache_pid, port),
            'bind_endpoint': {80: 'HTTP_80', 443: 'HTTPS_443', 8080: 'CONTROL_8080',
                              3000: 'MEDIA_3000'}.get(port, 'UNKNOWN' if port is None else 'OTHER'),
            'bind_config_source': _config_source(config, port)}


def main():
    print(json.dumps(run_diagnostic(), sort_keys=True))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
