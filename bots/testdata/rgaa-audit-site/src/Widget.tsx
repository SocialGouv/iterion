import React from "react";

export function Widget({ src }: { src: string }) {
  return (
    <section>
      <img src={src} />
      <a href="/ouvrir" target="_blank">Ouvrir</a>
      <button onClick={() => undefined}>
        <svg viewBox="0 0 10 10"><path d="M0 0h10v10H0z" /></svg>
      </button>
      <div role="button">Valider</div>
    </section>
  );
}
