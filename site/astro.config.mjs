import { defineConfig } from 'astro/config';
import sitemap from '@astrojs/sitemap';

// The landing page at https://chainmail.sh. The manual is a separate
// Docusaurus build served under /docs/ (see scripts/build-pages.sh).
export default defineConfig({
  site: 'https://chainmail.sh',
  trailingSlash: 'ignore',
  build: { format: 'file' },
  integrations: [sitemap()],
});
