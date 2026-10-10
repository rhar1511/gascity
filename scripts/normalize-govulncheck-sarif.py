#!/usr/bin/env python3
"""Remove only identical SARIF stack entries emitted by govulncheck."""

import argparse
import json
from pathlib import Path


def normalize(document):
    """Preserve every result and distinct trace; return duplicate stack count."""
    if document.get('version') != '2.1.0' or not isinstance(document.get('runs'), list):
        raise ValueError('expected a SARIF 2.1.0 runs array')
    removed = 0
    for run in document['runs']:
        results = run.get('results', [])
        if not isinstance(results, list):
            raise ValueError('expected a SARIF results array')
        for result in results:
            if 'stacks' not in result:
                continue
            stacks = result['stacks']
            if not isinstance(stacks, list):
                raise ValueError('expected a SARIF stacks array')
            seen = set()
            distinct = []
            for stack in stacks:
                key = json.dumps(stack, sort_keys=True, separators=(',', ':'))
                if key in seen:
                    removed += 1
                else:
                    seen.add(key)
                    distinct.append(stack)
            result['stacks'] = distinct
    return removed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    parser.add_argument('destination', type=Path)
    args = parser.parse_args()
    if args.source.resolve() == args.destination.resolve():
        parser.error('source and destination must differ; preserve the raw report')
    document = json.loads(args.source.read_text())
    removed = normalize(document)
    args.destination.write_text(json.dumps(document, indent=2) + '\n')
    print(f'SARIF: removed {removed} identical stack entries; findings unchanged')


if __name__ == '__main__':
    main()
