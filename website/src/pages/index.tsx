import { useState, type ReactNode } from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import Layout from '@theme/Layout';
import Mermaid from '@theme/Mermaid';

import styles from './index.module.css';

const installCommand = 'brew install yaklabco/tap/chainmail';

// The same diagram as the Introduction page and the README.
const architecture = `flowchart LR
  A[AI agent] -- MCP over stdio --> M[chainmail mcp]
  C[chainmail CLI] -- ConnectRPC --> S((Unix socket))
  M -- ConnectRPC --> S
  S --> D[chainmail daemon]
  D --- V[(vault)]
  D --- P{policy}
  D --- L[(audit log)]
  D --- K[(cache)]
  D -- IMAP --> I[mail servers]
  D -- SMTP --> I`;

type Feature = { title: string; body: ReactNode };

const features: Feature[] = [
  {
    title: 'Credentials stay in the daemon',
    body: (
      <>
        The daemon owns every password and token in an age-encrypted vault. Clients only reach it over a
        Unix socket.
      </>
    ),
  },
  {
    title: 'Risky sends are held',
    body: <>A send to anyone off the allowlist is saved as a draft for you to review, never sent.</>,
  },
  {
    title: 'Agents speak MCP',
    body: (
      <>
        <code>chainmail mcp</code> gives agents the same calls as the CLI, behind the same policy and audit log.
      </>
    ),
  },
];

function InstallBox(): ReactNode {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(installCommand).then(
      () => {
        setCopied(true);
        setTimeout(() => setCopied(false), 2000);
      },
      () => setCopied(false),
    );
  };
  return (
    <div className={styles.install}>
      <span className={styles.prompt} aria-hidden="true">
        $
      </span>
      <code>{installCommand}</code>
      <button
        type="button"
        className="button button--sm button--secondary"
        onClick={copy}
        aria-label="Copy the install command"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  );
}

function Hero(): ReactNode {
  const { siteConfig } = useDocusaurusContext();
  return (
    <header className={styles.hero}>
      <div className="container">
        <img
          className={styles.mascot}
          src={useBaseUrl('/img/mascot.png')}
          width={320}
          alt="The chainmail gopher: a Go gopher in mail armour with an envelope shield"
        />
        <h1 className={styles.title}>{siteConfig.title}</h1>
        <p className={styles.tagline}>{siteConfig.tagline}</p>
        <InstallBox />
        <div className={styles.buttons}>
          <Link className="button button--primary button--lg" to="/docs/getting-started/install">
            Get started
          </Link>
          <Link className="button button--secondary button--lg" href="https://github.com/yaklabco/chainmail">
            GitHub
          </Link>
        </div>
      </div>
    </header>
  );
}

function Features(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {features.map(({ title, body }) => (
            <div key={title} className={clsx('col col--4')}>
              <div className={styles.card}>
                <h2>{title}</h2>
                <p>{body}</p>
              </div>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}

function Architecture(): ReactNode {
  return (
    <section className={styles.diagram}>
      <div className="container">
        <h2>How it fits together</h2>
        <Mermaid value={architecture} />
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  const { siteConfig } = useDocusaurusContext();
  return (
    <Layout title="Email for AI agents" description={siteConfig.tagline}>
      <Hero />
      <main>
        <Features />
        <Architecture />
      </main>
    </Layout>
  );
}
