import React from 'react';
import ReactDOM from 'react-dom/client';
import '@semi/css';

import App from './App';
import './styles.css';

document.body.setAttribute('theme-mode', 'dark');

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
