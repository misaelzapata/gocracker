// Tests for the output comparison logic extracted from fanout.ts
// We test the pure logic inline since runFanOut depends on VS Code APIs

type Output = { slot: number; stdout: string; stderr: string; exitCode: number };

function allSame(outputs: Output[]): boolean {
  return outputs.length > 1 && outputs.every(o => o.stdout === outputs[0].stdout);
}

describe('fan-out output comparison', () => {
  test('identical outputs detected', () => {
    const outputs: Output[] = [
      { slot: 1, stdout: 'hello\n', stderr: '', exitCode: 0 },
      { slot: 2, stdout: 'hello\n', stderr: '', exitCode: 0 },
      { slot: 3, stdout: 'hello\n', stderr: '', exitCode: 0 },
    ];
    expect(allSame(outputs)).toBe(true);
  });

  test('differing outputs detected', () => {
    const outputs: Output[] = [
      { slot: 1, stdout: 'hello\n', stderr: '', exitCode: 0 },
      { slot: 2, stdout: 'world\n', stderr: '', exitCode: 0 },
    ];
    expect(allSame(outputs)).toBe(false);
  });

  test('single output is never "all same"', () => {
    const outputs: Output[] = [{ slot: 1, stdout: 'hello\n', stderr: '', exitCode: 0 }];
    expect(allSame(outputs)).toBe(false);
  });

  test('empty outputs array is never "all same"', () => {
    expect(allSame([])).toBe(false);
  });

  test('stderr differences do not affect stdout comparison', () => {
    const outputs: Output[] = [
      { slot: 1, stdout: 'hello\n', stderr: 'warn1', exitCode: 0 },
      { slot: 2, stdout: 'hello\n', stderr: 'warn2', exitCode: 0 },
    ];
    expect(allSame(outputs)).toBe(true);
  });
});
