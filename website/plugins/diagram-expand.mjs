/**
 * Docusaurus client module. Mermaid diagrams shrink to fit the text column.
 * When one is shown below 90% of its natural width, this adds an Expand
 * button that opens the diagram at full size in a dialog.
 *
 * Mermaid renders after the page loads and again when the colour mode
 * changes, so a MutationObserver watches for new diagrams. A resize check
 * adds or removes the button as the column width changes.
 */

const CONTAINER = '.docusaurus-mermaid-container';
const BUTTON_CLASS = 'diagram-expand';
const MIN_SCALE = 0.9;

export function shouldOfferExpand(natural, shown) {
  if (!(natural > 0) || !(shown > 0)) return false;
  return shown / natural < MIN_SCALE;
}

export function naturalSize(viewBox) {
  const parts = (viewBox ?? '').trim().split(/[\s,]+/).map(Number);
  if (parts.length !== 4 || parts.some((n) => Number.isNaN(n))) return { width: 0, height: 0 };
  return { width: parts[2], height: parts[3] };
}

let dialog = null;

function getDialog() {
  if (dialog) return dialog;
  dialog = document.createElement('dialog');
  dialog.className = 'diagram-dialog';
  dialog.setAttribute('aria-label', 'Diagram at full size');
  dialog.innerHTML =
    '<form method="dialog" class="diagram-dialog__bar"><button class="diagram-dialog__close" aria-label="Close">Close</button></form>' +
    '<div class="diagram-dialog__body"></div>';
  // A click on the backdrop lands on the dialog element itself.
  dialog.addEventListener('click', (event) => {
    if (event.target === dialog) dialog.close();
  });
  dialog.addEventListener('close', () => {
    dialog.querySelector('.diagram-dialog__body').replaceChildren();
  });
  document.body.append(dialog);
  return dialog;
}

function open(svg) {
  const { width, height } = naturalSize(svg.getAttribute('viewBox'));
  // The copy keeps the diagram's id, because Mermaid scopes the diagram's
  // <style> rules to it. The dialog empties itself on close.
  const copy = svg.cloneNode(true);
  copy.style.maxWidth = 'none';
  copy.setAttribute('width', String(width));
  copy.setAttribute('height', String(height));
  const d = getDialog();
  d.querySelector('.diagram-dialog__body').replaceChildren(copy);
  d.showModal();
}

function decorate(container) {
  const svg = container.querySelector(':scope > svg');
  const existing = container.querySelector(`:scope > .${BUTTON_CLASS}`);
  const offer = svg && shouldOfferExpand(naturalSize(svg.getAttribute('viewBox')).width, svg.getBoundingClientRect().width);
  if (!offer) {
    existing?.remove();
    return;
  }
  if (existing) return;
  const button = document.createElement('button');
  button.type = 'button';
  button.className = BUTTON_CLASS;
  button.textContent = 'Expand';
  button.setAttribute('aria-label', 'Show the diagram at full size');
  button.addEventListener('click', () => {
    const current = container.querySelector(':scope > svg');
    if (current) open(current);
  });
  container.append(button);
}

function decorateAll() {
  for (const container of document.querySelectorAll(CONTAINER)) decorate(container);
}

if (typeof document !== 'undefined' && typeof MutationObserver !== 'undefined') {
  let queued = false;
  const schedule = () => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      decorateAll();
    });
  };
  new MutationObserver(schedule).observe(document.documentElement, { childList: true, subtree: true });
  window.addEventListener('resize', schedule);
}
