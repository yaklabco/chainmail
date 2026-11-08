/**
 * Mermaid settings for the docs. Mermaid takes one set of options for both
 * colour modes, so every colour here is a CSS variable. src/css/custom.css
 * sets the variables for light and dark, and a diagram follows the page's
 * mode without a fresh render.
 *
 * Mermaid puts themeCSS after its own theme rules inside the diagram's
 * <style>, under the same #id prefix, so these rules win without !important.
 * Fonts set here also apply while Mermaid measures labels, so boxes fit
 * their text.
 */

const sans = "'Instrument Sans Variable', system-ui, sans-serif";
const mono = "'DM Mono', ui-monospace, SFMono-Regular, Menlo, monospace";

const themeCSS = `
  .node rect, .node circle, .node ellipse, .node polygon, .node path {
    fill: var(--cm-diagram-node);
    stroke: var(--cm-diagram-node-line);
    stroke-width: 1px;
  }
  .node .label, .node .label p, .node .label span, .nodeLabel {
    color: var(--cm-diagram-ink);
    fill: var(--cm-diagram-ink);
    font-family: ${sans};
    font-size: 14px;
    line-height: 1.35;
  }
  .cluster rect {
    fill: var(--cm-diagram-cluster);
    stroke: var(--cm-diagram-cluster-line);
    stroke-width: 1px;
    stroke-dasharray: 4 4;
  }
  .cluster-label span, .cluster-label text, .cluster span, .cluster text {
    color: var(--cm-diagram-accent);
    fill: var(--cm-diagram-accent);
    background-color: var(--cm-diagram-bg);
    font-family: ${mono};
    font-size: 12px;
  }
  .edgePaths .path, .flowchart-link {
    stroke: var(--cm-diagram-edge);
    stroke-width: 1.25px;
  }
  .marker, .arrowheadPath, marker path {
    fill: var(--cm-diagram-edge);
    stroke: var(--cm-diagram-edge);
  }
  .edgeLabel, .edgeLabel p, .edgeLabel span, .labelBkg {
    background-color: var(--cm-diagram-bg);
    color: var(--cm-diagram-muted);
    font-family: ${mono};
    font-size: 12px;
  }
  .edgeLabel rect {
    fill: var(--cm-diagram-bg);
    opacity: 1;
  }

  .actor {
    fill: var(--cm-diagram-node);
    stroke: var(--cm-diagram-node-line);
  }
  text.actor > tspan {
    fill: var(--cm-diagram-ink);
  }
  .actor-line {
    stroke: var(--cm-diagram-lifeline);
  }
  .messageLine0, .messageLine1 {
    stroke: var(--cm-diagram-edge);
    stroke-width: 1.25px;
  }
  [id$="-arrowhead"] path, [id$="-crosshead"] path {
    fill: var(--cm-diagram-edge);
    stroke: var(--cm-diagram-edge);
  }
  .messageText {
    fill: var(--cm-diagram-ink);
  }
  .note {
    fill: var(--cm-diagram-note);
    stroke: var(--cm-diagram-cluster-line);
  }
  .noteText, .noteText > tspan {
    fill: var(--cm-diagram-ink);
  }
  .labelBox {
    fill: var(--cm-diagram-node);
    stroke: var(--cm-diagram-cluster-line);
  }
  .labelText, .labelText > tspan, .loopText, .loopText > tspan, .sectionTitle, .sectionTitle > tspan {
    fill: var(--cm-diagram-accent);
  }
  .loopLine {
    stroke: var(--cm-diagram-cluster-line);
    fill: var(--cm-diagram-cluster-line);
  }
  .activation0, .activation1, .activation2 {
    fill: var(--cm-diagram-cluster);
    stroke: var(--cm-diagram-cluster-line);
  }
`;

/**
 * Diagrams shrink to fit the column (useMaxWidth) and never grow past their
 * natural size. plugins/diagram-expand.mjs offers the full size when one
 * shrinks a lot. Sequence diagrams wrap long messages, so their width
 * follows the number of participants more than the length of the text.
 *
 * @type {import('mermaid').MermaidConfig}
 */
export const mermaidOptions = {
  // Mermaid 12 defaults to the neo look, which adds a pale drop shadow.
  look: 'classic',
  layout: 'elk',
  fontFamily: sans,
  fontSize: 14,
  themeCSS,
  elk: { nodePlacementStrategy: 'NETWORK_SIMPLEX', mergeEdges: false },
  flowchart: { useMaxWidth: true, htmlLabels: true, padding: 14, nodeSpacing: 36, rankSpacing: 52 },
  sequence: {
    useMaxWidth: true,
    wrap: true,
    width: 130,
    mirrorActors: false,
    actorFontFamily: sans,
    actorFontSize: 14,
    messageFontFamily: mono,
    messageFontSize: 12,
    noteFontFamily: sans,
    noteFontSize: 13,
    boxMargin: 12,
    actorMargin: 32,
  },
};
