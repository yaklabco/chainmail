import type { Config } from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
import { themes as prismThemes } from 'prism-react-renderer';
import rehypeTables from './plugins/rehype-tables.mjs';
import { mermaidOptions } from './plugins/mermaid-theme.mjs';

const config: Config = {
  title: 'chainmail',
  tagline: 'Email for AI agents, behind a policy daemon',
  favicon: 'img/favicon.ico',

  url: 'https://chainmail.sh',
  baseUrl: '/docs/',

  organizationName: 'yaklabco',
  projectName: 'chainmail',

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  markdown: {
    // .md files are CommonMark and .mdx files are MDX, so a placeholder such
    // as <account> in a code block of a .md page is plain text.
    format: 'detect',
    mermaid: true,
    hooks: { onBrokenMarkdownLinks: 'throw', onBrokenMarkdownImages: 'throw' },
  },

  clientModules: ['./plugins/diagram-expand.mjs'],

  i18n: { defaultLocale: 'en', locales: ['en'] },

  headTags: [
    { tagName: 'link', attributes: { rel: 'icon', type: 'image/svg+xml', href: '/docs/img/favicon.svg' } },
    { tagName: 'link', attributes: { rel: 'icon', type: 'image/png', sizes: '192x192', href: '/docs/img/favicon-192.png' } },
    { tagName: 'link', attributes: { rel: 'apple-touch-icon', href: '/docs/img/apple-touch-icon.png' } },
  ],

  themes: [
    '@docusaurus/theme-mermaid',
    [
      '@easyops-cn/docusaurus-search-local',
      { hashed: true, indexBlog: false, docsRouteBasePath: '/' },
    ],
  ],

  presets: [
    [
      'classic',
      {
        docs: {
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/yaklabco/chainmail/edit/main/website/',
          rehypePlugins: [rehypeTables],
        },
        blog: false,
        theme: { customCss: './src/css/custom.css' },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/social-card.png',
    colorMode: { defaultMode: 'dark', respectPrefersColorScheme: true },
    navbar: {
      logo: {
        alt: 'chainmail',
        src: 'img/wordmark-light.svg',
        srcDark: 'img/wordmark-dark.svg',
        href: 'https://chainmail.sh/',
        target: '_self',
        width: 173,
        height: 22,
      },
      items: [
        { type: 'docSidebar', sidebarId: 'docs', position: 'left', label: 'Docs' },
        { to: '/reference/changelog', label: 'Changelog', position: 'left' },
        { href: 'https://github.com/yaklabco/chainmail', label: 'GitHub', position: 'right' },
        {
          href: 'https://chainmail.sh/#install',
          label: 'Get chainmail',
          position: 'right',
          className: 'navbar-cta',
          target: '_self',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            { label: 'Getting started', to: '/getting-started/install' },
            { label: 'User guide', to: '/guide/reading-and-searching' },
            { label: 'Operations', to: '/operations/deploy' },
            { label: 'Developer guide', to: '/developers/architecture' },
          ],
        },
        {
          title: 'Project',
          items: [
            { label: 'GitHub', href: 'https://github.com/yaklabco/chainmail' },
            { label: 'Releases', href: 'https://github.com/yaklabco/chainmail/releases' },
            { label: 'Changelog', to: '/reference/changelog' },
            { label: 'License', href: 'https://github.com/yaklabco/chainmail/blob/main/LICENSE' },
          ],
        },
      ],
      copyright: `Copyright © 2026 James Ainslie. MIT license.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.oneDark,
      additionalLanguages: ['bash', 'toml', 'nix', 'go', 'protobuf', 'json'],
    },
    // The stock themes cover anything plugins/mermaid-theme.mjs does not
    // restyle, in the right mode.
    mermaid: { theme: { light: 'neutral', dark: 'dark' }, options: mermaidOptions },
  } satisfies Preset.ThemeConfig,
};

export default config;
