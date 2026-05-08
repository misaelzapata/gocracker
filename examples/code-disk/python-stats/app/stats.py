#!/usr/bin/env python3
"""
Population statistics — reads /app/cities.csv from the code disk,
computes mean population, top-5 cities, and a simple histogram bucket.
Outputs JSON to stdout.
"""
import csv, json, sys

rows = []
with open('/app/cities.csv', newline='') as f:
    for r in csv.DictReader(f):
        try:
            rows.append({'city': r['city'], 'country': r['country'], 'pop': int(r['population'])})
        except (KeyError, ValueError):
            pass

if not rows:
    print('{"error": "no data"}'); sys.exit(1)

rows.sort(key=lambda r: r['pop'], reverse=True)
total = sum(r['pop'] for r in rows)
mean  = total // len(rows)

buckets = {'<1M': 0, '1–5M': 0, '5–10M': 0, '>10M': 0}
for r in rows:
    p = r['pop']
    if p < 1_000_000:       buckets['<1M']   += 1
    elif p < 5_000_000:     buckets['1–5M']  += 1
    elif p < 10_000_000:    buckets['5–10M'] += 1
    else:                   buckets['>10M']  += 1

print(json.dumps({
    'city_count':   len(rows),
    'total_pop':    total,
    'mean_pop':     mean,
    'top5':         rows[:5],
    'histogram':    buckets,
}, indent=2))
