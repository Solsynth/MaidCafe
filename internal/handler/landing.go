package handler

const cloudLandingPageHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="theme-color" content="#17212b">
  <meta name="description" content="MaidCafe cloud is ready for MaidKit.">
  <title>MaidCafe cloud is ready</title>
  <style>
    :root {
      color-scheme: light;
      --ink: #17212b;
      --muted: #5f6c76;
      --line: #d9e0e5;
      --surface: #ffffff;
      --background: #f4f7f9;
      --accent: #176b87;
      --accent-dark: #0d536a;
      --success: #247a59;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-width: 320px;
      background: var(--background);
      color: var(--ink);
      font: 16px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    main {
      width: min(920px, calc(100% - 32px));
      margin: 0 auto;
      padding: 48px 0 56px;
    }
    .brand {
      color: var(--muted);
      font-size: 0.9rem;
      font-weight: 700;
      letter-spacing: 0.08em;
      text-transform: uppercase;
    }
    .panel {
      margin-top: 18px;
      padding: clamp(28px, 6vw, 64px);
      background: var(--surface);
      border: 1px solid var(--line);
      border-radius: 18px;
      box-shadow: 0 12px 36px rgba(23, 33, 43, 0.07);
    }
    .status {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      color: var(--success);
      font-weight: 700;
    }
    .status::before {
      width: 9px;
      height: 9px;
      border-radius: 50%;
      background: currentColor;
      content: "";
    }
    h1 {
      max-width: 680px;
      margin: 18px 0 16px;
      font-size: clamp(2.2rem, 6vw, 4.5rem);
      line-height: 1.05;
      letter-spacing: -0.04em;
    }
    .lead {
      max-width: 650px;
      margin: 0;
      color: var(--muted);
      font-size: clamp(1.05rem, 2vw, 1.25rem);
    }
    .steps {
      display: grid;
      grid-template-columns: repeat(3, 1fr);
      gap: 16px;
      margin: 40px 0 0;
      padding: 0;
      list-style: none;
    }
    .step {
      padding: 20px;
      border: 1px solid var(--line);
      border-radius: 12px;
    }
    .step-number {
      display: block;
      color: var(--accent);
      font-size: 0.85rem;
      font-weight: 800;
      letter-spacing: 0.08em;
    }
    h2 {
      margin: 8px 0 6px;
      font-size: 1.05rem;
    }
    .step p { margin: 0; color: var(--muted); }
    .note {
      margin: 32px 0 0;
      padding: 16px 18px;
      border-left: 3px solid var(--accent);
      background: #f0f7fa;
      color: var(--muted);
    }
    .note strong { color: var(--ink); }
    footer {
      margin-top: 20px;
      color: var(--muted);
      font-size: 0.9rem;
      text-align: center;
    }
    @media (max-width: 700px) {
      main { padding-top: 28px; }
      .steps { grid-template-columns: 1fr; margin-top: 28px; }
      .panel { border-radius: 14px; }
    }
  </style>
</head>
<body>
  <main>
    <div class="brand">MaidCafe cloud</div>
    <section class="panel" aria-labelledby="title">
      <div class="status">Setup complete</div>
      <h1 id="title">Your MaidCafe cloud is ready.</h1>
      <p class="lead">
        This cloud service is running and ready to manage your MaidCafe daemons,
        metrics, and notifications. Use MaidKit to connect and finish setup.
      </p>
      <ol class="steps">
        <li class="step">
          <span class="step-number">01</span>
          <h2>Open MaidKit</h2>
          <p>Launch MaidKit on your desktop and sign in with your Solarpass account.</p>
        </li>
        <li class="step">
          <span class="step-number">02</span>
          <h2>Connect the cloud</h2>
          <p>Open Settings, choose MaidCafe, and use this cloud endpoint.</p>
        </li>
        <li class="step">
          <span class="step-number">03</span>
          <h2>Register a server</h2>
          <p>Open a server in MaidKit, select the MaidCafe tab, and register its daemon.</p>
        </li>
      </ol>
      <p class="note">
        <strong>Keep your daemon secret safe.</strong>
        MaidKit displays the enrollment secret only once. Copy it into the daemon
        configuration when prompted; it will not be returned by later requests.
      </p>
    </section>
    <footer>MaidCafe provides the cloud control plane for MaidKit-managed hosts.</footer>
  </main>
</body>
</html>
`
