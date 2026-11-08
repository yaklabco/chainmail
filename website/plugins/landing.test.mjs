// The landing page's stories and the /search page: routes, links and the
// benchmark figures the search page draws its chart from.
import { describe, expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const root = join(import.meta.dir, '..', '..');
const read = (p) => readFileSync(join(root, p), 'utf8');

describe('the /search page', () => {
  test('exists, and no redirect sends /search to the manual', () => {
    expect(existsSync(join(root, 'site/src/pages/search.astro'))).toBe(true);
    const redirects = join(root, 'site/public/_redirects');
    if (existsSync(redirects)) expect(readFileSync(redirects, 'utf8')).not.toMatch(/^\/search\s/m);
  });

  test('the nav and the landing page link to it', () => {
    expect(read('site/src/layouts/Base.astro')).toContain('href="/search"');
    expect(read('site/src/pages/index.astro')).toContain('href="/search"');
  });

  test('every benchmark row is measured, and the index is the faster path', () => {
    const rows = JSON.parse(read('site/src/data/search-bench.json')).rows;
    expect(rows.length).toBeGreaterThanOrEqual(5);
    for (const r of rows) {
      expect(r.label.length).toBeGreaterThan(0);
      expect(r.server_ms).toBeGreaterThan(r.cache_ms);
      expect(r.cache_ms).toBeGreaterThan(0);
    }
  });

  test('the hero figure matches the first benchmark row', () => {
    const [first] = JSON.parse(read('site/src/data/search-bench.json')).rows;
    expect(first.server_ms).toBe(2796);
    expect(Math.round(first.cache_ms)).toBe(88);
    expect(read('site/src/pages/search.astro')).toContain('2.8 s');
  });
});

describe('the landing page stories', () => {
  test('lead with what the agent does', () => {
    const page = read('site/src/pages/index.astro');
    expect(page).toContain('Hand your inbox to your AI agent');
    expect(page).toContain('id="stories"');
    expect(page.indexOf('id="stories"')).toBeLessThan(page.indexOf('id="policy"'));
  });

  test('mark the features that are not in a release yet', () => {
    const page = read('site/src/pages/index.astro');
    expect(page.match(/data-unreleased/g)?.length ?? 0).toBe(2);
  });

  test('leave the calendar out until it ships', () => {
    expect(read('site/src/pages/index.astro')).not.toMatch(/invite/i);
  });
});

describe('site copy', () => {
  test('has no em dashes', () => {
    for (const p of ['site/src/pages/index.astro', 'site/src/pages/search.astro', 'site/src/layouts/Base.astro']) {
      if (existsSync(join(root, p))) expect(read(p)).not.toContain('\u2014');
    }
  });
});
