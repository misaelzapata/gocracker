// Tests for extractCodeBlock — the only pure function in chat.ts
// We duplicate the function here to test it without importing VS Code

function extractCodeBlock(text: string): { lang: string; code: string } | null {
  const match = text.match(/```(\w*)\n([\s\S]*?)```/);
  if (!match) return null;
  return { lang: match[1].toLowerCase() || 'js', code: match[2] };
}

describe('extractCodeBlock', () => {
  test('extracts python block', () => {
    const result = extractCodeBlock('Run this:\n```python\nprint("hi")\n```');
    expect(result).toEqual({ lang: 'python', code: 'print("hi")\n' });
  });

  test('extracts js block without lang label → defaults to js', () => {
    const result = extractCodeBlock('```\nconsole.log(1)\n```');
    expect(result).toEqual({ lang: 'js', code: 'console.log(1)\n' });
  });

  test('returns null when no code block', () => {
    expect(extractCodeBlock('just some text')).toBeNull();
  });

  test('picks first block when multiple present', () => {
    const text = '```python\nfirst\n```\n```go\nsecond\n```';
    const result = extractCodeBlock(text);
    expect(result?.lang).toBe('python');
    expect(result?.code).toBe('first\n');
  });

  test('lowercases language identifier', () => {
    const result = extractCodeBlock('```TypeScript\nconst x = 1\n```');
    expect(result?.lang).toBe('typescript');
  });
});
