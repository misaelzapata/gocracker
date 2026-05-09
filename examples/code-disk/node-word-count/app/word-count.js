#!/usr/bin/env node
/**
 * Word frequency counter — reads /app/text.txt from the code disk,
 * returns the top-10 words and total count as JSON.
 */
const fs = require('fs');

const text = fs.readFileSync('/app/text.txt', 'utf8');
const words = text.toLowerCase().replace(/[^a-z\s'-]/g, ' ').match(/\b[a-z]{3,}\b/g) || [];

const freq = Object.create(null);
for (const w of words) freq[w] = (freq[w] ?? 0) + 1;

const top10 = Object.entries(freq)
  .sort((a, b) => b[1] - a[1])
  .slice(0, 10)
  .map(([word, count]) => ({ word, count }));

process.stdout.write(JSON.stringify({
  total_words: words.length,
  unique_words: Object.keys(freq).length,
  top10,
}, null, 2) + '\n');
