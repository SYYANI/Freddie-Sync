#!/usr/bin/env python3
"""Exercise the deployed HTTPS service using isolated, temporary device tokens."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import subprocess
import time
import urllib.error
import urllib.request
import uuid

from provision import admin


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', required=True, help='SSH destination of the sync server')
    parser.add_argument('--base-url', required=True, help='for example https://sync.example.com/freddie-sync/')
    args = parser.parse_args()
    if not args.base_url.startswith('https://'):
        raise ValueError('Smoke verification requires trusted HTTPS')
    base = args.base_url.rstrip('/')
    library = 'smoke-' + str(uuid.uuid4())
    created = []
    libraries = []

    def device(label, lib=library):
        result = admin(args.host, 'device-add', '--label', label, '--library', lib)
        created.append(result)
        return result

    def call(method, path, credential=None, body=None, headers=None):
        outgoing = dict(headers or {})
        if credential:
            outgoing['Authorization'] = 'Bearer ' + credential['token']
        if isinstance(body, dict):
            body = json.dumps(body).encode()
            outgoing['Content-Type'] = 'application/json'
        elif body is not None:
            outgoing['Content-Type'] = 'application/octet-stream'
        req = urllib.request.Request(base + path, data=body, headers=outgoing, method=method)
        try:
            response = urllib.request.urlopen(req, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            value = json.loads(data) if response.headers.get_content_type() == 'application/json' and data else data
            return response.status, value, response.headers

    def expect(status, response):
        if response[0] != status:
            raise AssertionError(f'Expected HTTP {status}, got {response[0]}: {response[1]}')
        return response[1]

    def batch(ident, revision=0, deleted=False):
        return {'requestID': str(uuid.uuid4()), 'mutations': [
            {'kind': 'paper', 'id': ident, 'baseRevision': revision,
             'deleted': deleted, 'payload': None if deleted else {'title': 'Integration test'}, 'blobs': []}]}

    try:
        for lib in [library, library + '-other']:
            admin(args.host, 'library-add', '--name', 'Smoke test', '--id', lib)
            libraries.append(lib)
        a, b, other = device('Integration Mac'), device('Integration iPad'), device('Isolated library', library + '-other')
        expect(200, call('GET', '/health'))
        expect(401, call('GET', '/v1/info'))
        expect(403, call('GET', '/v1/info', a, headers={'Origin': 'https://example.com'}))
        expect(404, call('GET', '/devices', a))
        info = expect(200, call('GET', '/v1/info', a))
        assert info['libraryID'] == library and info['protocolVersion'] == 1
        # A realistic-sized attachment exercises streaming across nginx and the service.
        prefix = b'%PDF-1.7\nFreddie sync integration fixture\n'
        data = prefix + b'0123456789abcdef' * (512 * 1024)
        digest = hashlib.sha256(data).hexdigest()
        expect(422, call('PUT', '/v1/blobs/' + '0' * 64, a, data))
        expect(201, call('PUT', '/v1/blobs/' + digest, a, data))
        expect(200, call('PUT', '/v1/blobs/' + digest, a, data))
        assert expect(200, call('GET', '/v1/blobs/' + digest, b)) == data
        assert expect(206, call('GET', '/v1/blobs/' + digest, b, headers={'Range': 'bytes=0-3'})) == b'%PDF'
        expect(404, call('GET', '/v1/blobs/' + digest, other))
        push = batch('paper-a')
        push['mutations'][0]['blobs'] = [digest]
        receipt = expect(200, call('POST', '/v1/changes', a, push))
        assert expect(200, call('POST', '/v1/changes', a, push)) == receipt
        expect(409, call('POST', '/v1/changes', b, batch('paper-a')))
        expect(200, call('GET', '/v1/records/paper/paper-a', b))
        expect(404, call('GET', '/v1/records/paper/paper-a', other))
        base_revision = receipt['revisions'][0]
        def writer(credential):
            return call('POST', '/v1/changes', credential, batch('paper-a', base_revision))
        with ThreadPoolExecutor(max_workers=2) as executor:
            responses = list(executor.map(writer, [a, b]))
        assert sorted(response[0] for response in responses) == [200, 409]
        current = expect(200, call('GET', '/v1/records/paper/paper-a', a))
        deletion = expect(200, call('POST', '/v1/changes', b, batch('paper-a', current['revision'], True)))
        expect(409, call('POST', '/v1/changes', a, batch('paper-a', deletion['revisions'][0])))
        feed = expect(200, call('GET', '/v1/changes?after=0&limit=1', b))
        assert feed['hasMore'] and feed['cursor'] == 1
        last = expect(200, call('GET', f'/v1/changes?after={feed["cursor"]}&until={feed["headRevision"]}', b))
        assert last['changes'][-1]['deleted'] and not last['hasMore']
        # A lost response followed by a service restart must not duplicate the commit.
        subprocess.run(['ssh', '-o', 'BatchMode=yes', args.host, 'systemctl restart freddie-sync.service'], check=True, timeout=45)
        for _ in range(30):
            try:
                if call('GET', '/health')[0] == 200:
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.3)
        else:
            raise AssertionError('Service failed to recover after restart')
        assert expect(200, call('POST', '/v1/changes', a, push)) == receipt
        assert expect(200, call('GET', '/v1/records/paper/paper-a', b))['deleted']
        assert expect(200, call('GET', '/v1/info', a))['storeID'] == info['storeID']
        admin(args.host, 'device-revoke', '--id', b['device']['id'])
        expect(401, call('GET', '/v1/info', b))
        print('HTTPS, authentication, origin checks, file integrity/ranges, two-device sync,')
        print('library isolation, concurrent conflicts, tombstones, retry after restart, revocation: PASS')
    finally:
        for credential in created:
            admin(args.host, 'device-revoke', '--id', credential['device']['id'])
        for lib in libraries:
            admin(args.host, 'library-delete', '--id', lib)


if __name__ == '__main__':
    main()
