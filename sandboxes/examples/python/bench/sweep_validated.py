"""Large sweep: 50 docker-hub images sampled from docs/VALIDATED_PROJECTS.md.
Each: cold-boot via gocracker → exec a version command → verify → delete.
Records full timing breakdown."""
import sys, time, json
sys.path.insert(0, '/home/misael/Desktop/projects/gocracker/sandboxes/sdk/python')
from gocracker import Client, ProcessExitError, SandboxError

c = Client('http://127.0.0.1:9091', timeout=600)
KERNEL = '/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux'

# (image, cmd, expected substring) — picked from the 378-project list,
# preferring images <300MB with predictable version commands.
IMAGES = [
    # Go services (matches docs section)
    ('traefik/whoami:latest',          ['/whoami', '--help'],                     'whoami'),
    ('caddy:2-alpine',                 ['caddy', 'version'],                      'v2.'),
    ('traefik:latest',                 ['traefik', 'version'],                    'Version:'),
    ('hashicorp/consul:latest',        ['consul', 'version'],                     'Consul v'),
    ('hashicorp/vault:latest',         ['vault', 'version'],                      'Vault'),
    ('hashicorp/http-echo:latest',     ['http-echo', '-version'],                 'http-echo'),
    ('coredns/coredns:latest',         ['/coredns', '-version'],                  'CoreDNS'),
    ('jaegertracing/all-in-one:latest',['/go/bin/all-in-one-linux', 'help'],      'jaeger'),
    ('prom/prometheus:latest',         ['/bin/prometheus', '--version'],          'prometheus'),
    ('prom/alertmanager:latest',       ['/bin/alertmanager', '--version'],        'alertmanager'),
    ('prom/node-exporter:latest',      ['/bin/node_exporter', '--version'],       'node_exporter'),
    ('prom/blackbox-exporter:latest',  ['/bin/blackbox_exporter', '--version'],   'blackbox_exporter'),
    ('grafana/grafana:latest',         ['/usr/share/grafana/bin/grafana', 'cli', '-v'], 'grafana'),
    ('grafana/loki:latest',            ['/usr/bin/loki', '--version'],            'loki'),
    ('minio/minio:latest',             ['minio', '--version'],                    'minio version'),
    ('influxdb:2-alpine',              ['influxd', 'version'],                    'InfluxDB'),
    ('telegraf:alpine',                ['telegraf', '--version'],                 'Telegraf'),
    ('cockroachdb/cockroach:latest',   ['/cockroach/cockroach', 'version', '--build-tag'], 'v'),
    ('pocketbase/pocketbase:latest',   ['/usr/local/bin/pocketbase', '--version'],'pocketbase'),
    ('gotify/server:latest',           ['/app/gotify-app', '--version'],          ''),
    ('etcd:latest',                    ['etcd', '--version'],                     'etcd Version'),
    # Web servers / proxies / MQTT
    ('nginx:alpine',                   ['nginx', '-v'],                           'nginx version'),
    ('httpd:alpine',                   ['/usr/local/apache2/bin/httpd', '-v'],    'Server version: Apache'),
    ('haproxy:lts-alpine',             ['haproxy', '-v'],                         'HAProxy version'),
    ('eclipse-mosquitto:2',            ['mosquitto', '-h'],                       'mosquitto version'),
    # Databases
    ('redis:alpine',                   ['redis-server', '--version'],             'Redis server'),
    ('postgres:16-alpine',             ['postgres', '--version'],                 'postgres (PostgreSQL)'),
    ('mariadb:lts',                    ['mariadbd', '--version'],                 'mariadbd  Ver'),
    ('memcached:alpine',               ['memcached', '-h'],                       'memcached'),
    ('mongo:7',                        ['mongod', '--version'],                   'db version'),
    # Runtimes
    ('python:3.12-alpine',             ['python3', '--version'],                  'Python 3.12'),
    ('node:22-alpine',                 ['node', '-v'],                            'v22'),
    ('golang:1.23-alpine',             ['/usr/local/go/bin/go', 'version'],       'go1.23'),
    ('ruby:3-alpine',                  ['ruby', '--version'],                     'ruby 3.'),
    ('rust:1-slim',                    ['/usr/local/cargo/bin/rustc', '--version'], 'rustc'),
    ('php:8-cli-alpine',               ['php', '--version'],                      'PHP 8.'),
    ('elixir:alpine',                  ['elixir', '--version'],                   'Erlang/OTP'),
    ('eclipse-temurin:21-jre-alpine',  ['java', '-version'],                      'OpenJDK Runtime'),
    # Misc / CLIs
    ('alpine:3.20',                    ['cat', '/etc/alpine-release'],            '3.20'),
    ('busybox:latest',                 ['sh', '-c', 'busybox 2>&1 | head -1'],    'BusyBox'),
    ('debian:12-slim',                 ['cat', '/etc/debian_version'],            '12.'),
    ('ubuntu:24.04',                   ['cat', '/etc/lsb-release'],               '24.04'),
    ('amazonlinux:2023',               ['cat', '/etc/system-release'],            'Amazon Linux'),
    ('rockylinux:9-minimal',           ['cat', '/etc/rocky-release'],             'Rocky Linux'),
    ('mailhog/mailhog:latest',         ['MailHog', '-help'],                      'MailHog'),
    ('linuxserver/syslog-ng:latest',   ['syslog-ng', '--version'],                'syslog-ng'),
    ('drone/drone:latest',             ['/bin/drone-server', '--version'],        ''),
    ('jenkins/jenkins:lts-jdk21',      ['java', '-version'],                      'OpenJDK Runtime'),
    ('plantuml/plantuml-server:tomcat',['java', '-version'],                      'OpenJDK Runtime'),
    ('gitea/gitea:latest',             ['/usr/local/bin/gitea', '--version'],     'Gitea version'),
]

results = []
print(f'{"image":40} {"create":>8} {"exec":>7} {"del":>5}  status')
print('-' * 90)
for image, cmd, expected in IMAGES:
    rec = {'image': image, 'cmd': ' '.join(cmd), 'expected': expected}
    try:
        t0 = time.perf_counter()
        sb = c.create_sandbox(image=image, kernel_path=KERNEL, network_mode='auto', cmd=['sleep','infinity'])
        rec['create_ms'] = round((time.perf_counter() - t0) * 1000)
        t1 = time.perf_counter()
        try:
            r = sb.process.exec(cmd, timeout=120)
            rec['exec_ms'] = round((time.perf_counter() - t1) * 1000)
            combined = (r.stdout_text + r.stderr_text).strip()
            rec['ok'] = expected in combined if expected else r.exit_code == 0
            rec['output'] = combined[:80]
        except ProcessExitError as e:
            rec['exec_ms'] = round((time.perf_counter() - t1) * 1000)
            combined = (e.stdout + e.stderr).strip()
            rec['ok'] = expected in combined if expected else False
            rec['output'] = combined[:80]
            rec['exit'] = e.exit_code
        t2 = time.perf_counter()
        try: c.delete(sb.id)
        except: pass
        rec['delete_ms'] = round((time.perf_counter() - t2) * 1000)
        status = 'OK' if rec.get('ok') else f'FAIL ({rec.get("output","")[:50]})'
        print(f'{image:40} {rec.get("create_ms",0):>6}ms {rec.get("exec_ms",0):>5}ms {rec.get("delete_ms",0):>3}ms  {status}')
    except Exception as e:
        rec['error'] = f'{type(e).__name__}: {str(e)[:200]}'
        print(f'{image:40}  ERROR  {rec["error"][:60]}')
    results.append(rec)
with open('/tmp/sweep_large_results.json', 'w') as f:
    json.dump(results, f, indent=2)
oks = sum(1 for r in results if r.get('ok'))
print(f'\n=== {oks}/{len(results)} OK ===')
