import { describe, expect, test } from 'bun:test';
import rehypeTables from './rehype-tables.mjs';

const text = (value) => ({ type: 'text', value });
const el = (tagName, children = [], properties = {}) => ({ type: 'element', tagName, properties, children });
const code = (value) => el('code', [text(value)]);
const row = (cell, values) => el('tr', values.map((v) => el(cell, typeof v === 'string' ? [text(v)] : [v])));
const table = (head, ...body) => el('table', [el('thead', [row('th', head)]), el('tbody', body.map((r) => row('td', r)))]);
const root = (...children) => ({ type: 'root', children });

const run = (tree) => {
  rehypeTables()(tree);
  return tree;
};
const cells = (tbl) => tbl.children[1].children.flatMap((r) => r.children);

describe('rehype-tables', () => {
  test('wraps each table in a scroll container', () => {
    const tree = run(root(table(['A', 'B', 'C'], ['1', '2', '3'])));
    const [wrap] = tree.children;
    expect(wrap.tagName).toBe('div');
    expect(wrap.properties.className).toEqual(['table-wrap']);
    expect(wrap.children[0].tagName).toBe('table');
  });

  test('wraps a table nested inside another element', () => {
    const tree = run(root(el('section', [table(['A', 'B', 'C'], ['1', '2', '3'])])));
    expect(tree.children[0].children[0].properties.className).toEqual(['table-wrap']);
  });

  test('turns a bare yes or no cell into a flag that keeps the word for screen readers', () => {
    const tree = run(root(table(['Tool', 'Read-only', 'Destructive'], [code('trash'), 'no', 'yes'])));
    const [, no, yes] = cells(tree.children[0].children[0]);
    expect(no.properties.className).toEqual(['cell-flag', 'cell-flag--no']);
    expect(yes.properties.className).toEqual(['cell-flag', 'cell-flag--yes']);
    const [mark, word] = yes.children;
    expect(mark.properties).toEqual({ className: ['flag'], ariaHidden: 'true' });
    expect(word.properties.className).toEqual(['flag__text']);
    expect(word.children).toEqual([text('yes')]);
  });

  test('leaves header cells and longer answers alone', () => {
    const tree = run(root(table(['yes', 'no', 'Note'], ['a', 'with expunge allowed', ' Yes '])));
    const tbl = tree.children[0].children[0];
    expect(tbl.children[0].children[0].children[0].properties).toEqual({});
    for (const cell of cells(tbl)) expect(cell.properties).toEqual({});
  });

  test('marks a two-column table whose rows each start with code as a term list', () => {
    const tree = run(root(table(['Package', 'What it owns'], [code('internal/api'), 'The contract'], [code('internal/cli'), 'The CLI'])));
    expect(tree.children[0].children[0].properties.className).toEqual(['table--terms']);
  });

  test('does not mark a table as a term list when a row starts with prose or it has more columns', () => {
    const prose = run(root(table(['Process', 'What'], [code('a'), 'x'], ['plain', 'y'])));
    expect(prose.children[0].children[0].properties.className).toBeUndefined();
    const wide = run(root(table(['A', 'B', 'C'], [code('a'), 'x', 'y'])));
    expect(wide.children[0].children[0].properties.className).toBeUndefined();
  });

  test('ignores whitespace text beside the code in a term cell', () => {
    const term = el('td', [text(' '), code('internal/api'), text('\n')]);
    const tbl = el('table', [el('thead', [row('th', ['Package', 'What'])]), el('tbody', [el('tr', [term, el('td', [text('x')])])])]);
    const tree = run(root(tbl));
    expect(tree.children[0].children[0].properties.className).toEqual(['table--terms']);
  });
});
