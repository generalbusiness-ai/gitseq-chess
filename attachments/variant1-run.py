import json
import pathlib
import subprocess

root = pathlib.Path(__file__).resolve().parent
binary, repo = root / 'chess', root / 'game-data'
transcript = []

def cli(*args):
    command = [str(binary), *args]
    result = subprocess.run(command, check=True, capture_output=True, text=True)
    data = json.loads(result.stdout)
    transcript.append({'transport': 'cli', 'argv': command, 'response': data})
    return data

init = cli('init', '--repo', str(repo))
created = cli('create', '--repo', str(repo), '--color', 'white', '--name', 'MCP acceptance', '--idempotency-key', 'accept-create')
assert created['effective'] is True, created
game = created['game']
process = subprocess.Popen([str(binary), 'mcp', '--repo', str(repo), '--key', str(root / 'black.key')], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
sequence = 0

def rpc(method, params):
    global sequence
    sequence += 1
    request = {'jsonrpc': '2.0', 'id': sequence, 'method': method, 'params': params}
    process.stdin.write(json.dumps(request) + '\n')
    process.stdin.flush()
    response = json.loads(process.stdout.readline())
    transcript.append({'transport': 'mcp', 'request': request, 'response': response})
    assert 'error' not in response, response
    assert not response['result'].get('isError'), response
    return response['result']

def call(name, **arguments):
    return rpc('tools/call', {'name': name, 'arguments': arguments})['structuredContent']

try:
    rpc('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {}, 'clientInfo': {'name': 'codex-acceptance', 'version': '1'}})
    process.stdin.write(json.dumps({'jsonrpc': '2.0', 'method': 'notifications/initialized'}) + '\n')
    process.stdin.flush()
    listed = rpc('tools/list', {})
    assert {'join', 'move', 'show_board', 'legal_destinations'}.issubset({tool['name'] for tool in listed['tools']})
    joined = call('join', game=game, idempotency_key='accept-join')
    assert joined['effective'] is True, joined
    for white, black, square in [('f2f3', 'e7e5', 'e7'), ('g2g4', 'd8h4', 'd8')]:
        moved = cli('move', '--repo', str(repo), '--game', game, '--move', white, '--idempotency-key', 'accept-' + white)
        assert moved['effective'] is True, moved
        legal = call('legal_destinations', game=game, **{'from': square})
        assert black[2:4] in legal['destinations'], legal
        moved = call('move', game=game, move=black, idempotency_key='accept-' + black)
        assert moved['effective'] is True, moved
    board = call('show_board', game=game)
    transcript.append({'acceptance': 'final MCP board', 'board': board})
finally:
    process.stdin.close()
    process.wait(timeout=10)
    assert process.returncode == 0, process.stderr.read()
    (root / 'transcript.json').write_text(json.dumps(transcript, indent=2) + '\n')

final = cli('board', '--repo', str(repo), '--game', game)
(root / 'transcript.json').write_text(json.dumps(transcript, indent=2) + '\n')
(root / 'result.json').write_text(json.dumps({'genesis': init['genesis'], 'game': game, 'board': final, 'cli_invocations': sum(x.get('transport') == 'cli' for x in transcript), 'mcp_requests': sequence}, indent=2) + '\n')
print(json.dumps(final, indent=2))
