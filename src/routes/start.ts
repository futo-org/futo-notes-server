import { Hono } from 'hono'

export const startRoutes = new Hono()

const HTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Stonefruit — Create Account</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <style>
    :root { color-scheme: light dark; }
    body {
      font-family: system-ui, -apple-system, sans-serif;
      max-width: 420px;
      margin: 4rem auto;
      padding: 0 1.5rem;
      line-height: 1.5;
    }
    h1 { margin-bottom: 0.25rem; }
    p.lead { color: #666; margin-top: 0; }
    form { display: flex; flex-direction: column; gap: 1rem; margin-top: 2rem; }
    label { display: flex; flex-direction: column; gap: 0.25rem; font-size: 0.9rem; }
    input {
      padding: 0.5rem 0.75rem;
      font-size: 1rem;
      border: 1px solid #ccc;
      border-radius: 6px;
      font-family: inherit;
    }
    button {
      padding: 0.6rem 0.75rem;
      font-size: 1rem;
      border: 0;
      border-radius: 6px;
      background: #2a6dd9;
      color: white;
      cursor: pointer;
    }
    button:disabled { opacity: 0.6; cursor: wait; }
    .error { color: #b22; font-size: 0.9rem; }
    .success { border: 1px solid #4a4; background: #eff; padding: 1rem; border-radius: 6px; }
    .success code { background: #dfd; padding: 0.1rem 0.3rem; border-radius: 3px; }
    @media (prefers-color-scheme: dark) {
      body { background: #111; color: #eee; }
      p.lead { color: #aaa; }
      input { background: #222; color: #eee; border-color: #444; }
      .success { background: #1a2a1a; border-color: #4a4; }
      .success code { background: #234; }
    }
  </style>
</head>
<body>
  <h1>Stonefruit</h1>
  <p class="lead">Create an account on this server.</p>

  <form id="signup">
    <label>
      Email
      <input type="email" name="email" required autocomplete="email">
    </label>
    <label>
      Password (min 8 characters)
      <input type="password" name="password" required minlength="8" autocomplete="new-password">
    </label>
    <label>
      Display name (optional)
      <input type="text" name="name" autocomplete="name">
    </label>
    <div class="error" id="error"></div>
    <button type="submit">Create account</button>
  </form>

  <div class="success" id="success" style="display:none; margin-top: 2rem;">
    <strong>Account created.</strong>
    <p>Open Stonefruit on your phone or computer, go to <strong>Settings &rarr; Sync</strong>, and enter:</p>
    <ul>
      <li>Server URL: <code id="server-url"></code></li>
      <li>Email and password you just entered</li>
    </ul>
  </div>

  <script>
    const form = document.getElementById('signup');
    const errorEl = document.getElementById('error');
    const successEl = document.getElementById('success');
    const serverUrlEl = document.getElementById('server-url');

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      errorEl.textContent = '';
      const button = form.querySelector('button');
      button.disabled = true;
      const data = Object.fromEntries(new FormData(form));
      try {
        const res = await fetch('/api/auth/signup', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(data),
        });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          errorEl.textContent = body.error || ('Signup failed (' + res.status + ')');
          button.disabled = false;
          return;
        }
        form.style.display = 'none';
        serverUrlEl.textContent = window.location.origin;
        successEl.style.display = 'block';
      } catch (err) {
        errorEl.textContent = String(err);
        button.disabled = false;
      }
    });
  </script>
</body>
</html>
`

startRoutes.get('/start', (c) => c.html(HTML))
