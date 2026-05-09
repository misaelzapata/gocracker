import { templateForDocument, execCommandForTemplate, SUPPORTED_EXTENSIONS } from '../language';

function fakeDoc(languageId: string, fileName: string) {
  return { languageId, fileName } as any;
}

describe('templateForDocument', () => {
  test.each([
    ['python',     'app.py',    'base-python'],
    ['javascript', 'index.js',  'base-node'],
    ['typescript', 'index.ts',  'base-bun'],
    ['go',         'main.go',   'base-go'],
    ['javascript', 'script.mjs','base-node'],
  ])('languageId=%s file=%s → %s', (lang, file, expected) => {
    expect(templateForDocument(fakeDoc(lang, file))).toBe(expected);
  });

  test('returns undefined for unsupported language', () => {
    expect(templateForDocument(fakeDoc('rust', 'main.rs'))).toBeUndefined();
  });
});

describe('execCommandForTemplate', () => {
  test('python3 for base-python', () => {
    const cmd = execCommandForTemplate('base-python', '/tmp/x.py');
    expect(cmd).toEqual(['python3', '/tmp/x.py']);
  });

  test('node for base-node', () => {
    const cmd = execCommandForTemplate('base-node', '/tmp/x.js');
    expect(cmd[0]).toBe('node');
    expect(cmd).toContain('/tmp/x.js');
  });

  test('go run for base-go', () => {
    const cmd = execCommandForTemplate('base-go', '/tmp/x.go');
    expect(cmd).toEqual(['go', 'run', '/tmp/x.go']);
  });
});

describe('SUPPORTED_EXTENSIONS', () => {
  test('includes .py .js .ts .go', () => {
    expect(SUPPORTED_EXTENSIONS).toEqual(expect.arrayContaining(['.py', '.js', '.ts', '.go']));
  });
});
