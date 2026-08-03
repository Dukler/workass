import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './styles/tokens.css';
import './styles/app.css';
import { App } from './components/App';
import { store } from './store/store';

// Kick off boot (wire events, hydrate from server, load archives). The local
// mirror already seeded the store synchronously in the constructor, so the
// first paint is instant.
void store.init();

const root = document.getElementById('root');
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
