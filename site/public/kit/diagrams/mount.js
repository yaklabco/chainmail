/*
  diagrams/mount.js - draws every [data-diagram] on the page. One line in the page:
    <link rel="stylesheet" href="assets/diagrams/diagram.css">
    <script type="module" src="assets/diagrams/mount.js"></script>
  The icons a diagram names are SVG files read from assets/icons/ or from data-icon-base.
*/
import dg from './diagram.js';

const go = () => dg.auto(document);
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', go, { once: true });
else go();
