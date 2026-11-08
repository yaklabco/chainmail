/**
 * Rehype plugin for the docs' Markdown tables. It changes markup only. The
 * look lives in src/css/custom.css.
 *
 * - Every table goes in a div.table-wrap, which scrolls sideways on narrow
 *   screens and carries the panel border.
 * - A body cell that holds only "yes" or "no" becomes a flag cell. CSS draws
 *   a check or a dash, and the word stays in the cell for screen readers,
 *   search and copy and paste.
 * - A two-column table whose every body row starts with inline code is a list
 *   of terms (a package and what it owns, a key and what it sets). It gets
 *   the class table--terms, which keeps the term on one line and stacks the
 *   rows on phones.
 */

const isElement = (node, tagName) => node?.type === 'element' && (!tagName || node.tagName === tagName);
const elements = (node, tagName) => (node.children ?? []).filter((child) => isElement(child, tagName));
const isBlank = (node) => node.type === 'text' && node.value.trim() === '';

function rows(tableNode) {
  return elements(tableNode).flatMap((section) => (section.tagName === 'tr' ? [section] : elements(section, 'tr')));
}

function bodyRows(tableNode) {
  return elements(tableNode, 'tbody').flatMap((body) => elements(body, 'tr'));
}

function cellsOf(rowNode) {
  return elements(rowNode).filter((cell) => cell.tagName === 'td' || cell.tagName === 'th');
}

function flagCell(cell) {
  const [only, ...rest] = cell.children;
  if (rest.length > 0 || only?.type !== 'text') return;
  const word = only.value;
  if (word !== 'yes' && word !== 'no') return;
  cell.properties = { ...cell.properties, className: ['cell-flag', `cell-flag--${word}`] };
  cell.children = [
    { type: 'element', tagName: 'span', properties: { className: ['flag'], ariaHidden: 'true' }, children: [] },
    { type: 'element', tagName: 'span', properties: { className: ['flag__text'] }, children: [only] },
  ];
}

function startsWithCode(cell) {
  const content = cell.children.filter((child) => !isBlank(child));
  return content.length === 1 && isElement(content[0], 'code');
}

function isTermList(tableNode) {
  const [header] = rows(tableNode);
  const body = bodyRows(tableNode);
  if (!header || cellsOf(header).length !== 2 || body.length === 0) return false;
  return body.every((r) => {
    const cells = cellsOf(r);
    return cells.length === 2 && startsWithCode(cells[0]);
  });
}

function decorate(tableNode) {
  if (isTermList(tableNode)) {
    tableNode.properties = { ...tableNode.properties, className: ['table--terms'] };
  }
  for (const r of bodyRows(tableNode)) {
    for (const cell of cellsOf(r)) if (cell.tagName === 'td') flagCell(cell);
  }
  return {
    type: 'element',
    tagName: 'div',
    properties: { className: ['table-wrap'] },
    children: [tableNode],
  };
}

function walk(parent) {
  if (!parent.children) return;
  parent.children = parent.children.map((child) => {
    if (isElement(child, 'table')) return decorate(child);
    walk(child);
    return child;
  });
}

export default function rehypeTables() {
  return (tree) => {
    walk(tree);
  };
}
