(() => {
  const canvas = document.getElementById("stage3d");
  if (!canvas) return;
  const ctx = canvas.getContext("2d", { alpha: true });
  if (!ctx) return;
  const reduced = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
  let w = 0, h = 0, dpr = 1, t = 0, mx = 0.5, my = 0.5, raf = 0;
  const dots = [];

  const theme = () => {
    const dark = document.documentElement.getAttribute("data-theme") !== "light";
    return dark
      ? { bg: "#020617", a: "56,189,248", b: "14,165,233", c: "29,78,216", grid: "56,189,248" }
      : { bg: "#e8eef7", a: "29,78,216", b: "2,132,199", c: "30,58,138", grid: "29,78,216" };
  };

  function seed() {
    dots.length = 0;
    const n = Math.max(36, Math.floor((w * h) / 28000));
    for (let i = 0; i < n; i++) {
      dots.push({
        x: Math.random() * w,
        y: Math.random() * h,
        z: Math.random() * 1.2 + 0.25,
        r: Math.random() * 1.6 + 0.4,
        vx: (Math.random() - 0.5) * 0.22,
        vy: (Math.random() - 0.5) * 0.18,
        p: Math.random() * 6.28,
      });
    }
  }

  function resize() {
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    w = window.innerWidth;
    h = window.innerHeight;
    canvas.width = Math.floor(w * dpr);
    canvas.height = Math.floor(h * dpr);
    canvas.style.width = w + "px";
    canvas.style.height = h + "px";
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    seed();
  }

  function frame(now) {
    t = now * 0.001;
    const col = theme();
    const px = (mx - 0.5) * 24;
    const py = (my - 0.5) * 16;
    ctx.clearRect(0, 0, w, h);
    ctx.fillStyle = col.bg;
    ctx.fillRect(0, 0, w, h);

    // orbs
    [[0.15, 0.12, 320, col.a, 0.14], [0.85, 0.2, 280, col.b, 0.1], [0.5, 0.95, 360, col.c, 0.08]].forEach(([fx, fy, R, rgb, a], i) => {
      const x = fx * w + Math.sin(t * 0.35 + i) * 16 + px * (0.3 + i * 0.08);
      const y = fy * h + Math.cos(t * 0.28 + i) * 12 + py * 0.25;
      const g = ctx.createRadialGradient(x, y, 0, x, y, R);
      g.addColorStop(0, `rgba(${rgb},${a})`);
      g.addColorStop(0.55, `rgba(${rgb},${a * 0.25})`);
      g.addColorStop(1, `rgba(${rgb},0)`);
      ctx.fillStyle = g;
      ctx.beginPath();
      ctx.arc(x, y, R, 0, Math.PI * 2);
      ctx.fill();
    });

    // perspective floor grid
    ctx.save();
    ctx.translate(w / 2 + px * 0.2, h * 0.78 + py * 0.15);
    ctx.transform(1, 0, 0, 0.42, 0, 0);
    ctx.strokeStyle = `rgba(${col.grid},0.06)`;
    ctx.lineWidth = 1;
    for (let i = -18; i <= 18; i++) {
      ctx.beginPath();
      ctx.moveTo(i * 44, -700);
      ctx.lineTo(i * 44, 700);
      ctx.stroke();
      ctx.beginPath();
      ctx.moveTo(-700, i * 44 + ((t * 28) % 44));
      ctx.lineTo(700, i * 44 + ((t * 28) % 44));
      ctx.stroke();
    }
    ctx.restore();

    // links + particles
    for (let i = 0; i < dots.length; i++) {
      const a = dots[i];
      if (!reduced) {
        a.x += a.vx * a.z;
        a.y += a.vy * a.z;
        if (a.x < -10) a.x = w + 10;
        if (a.x > w + 10) a.x = -10;
        if (a.y < -10) a.y = h + 10;
        if (a.y > h + 10) a.y = -10;
      }
      const ax = a.x + px * a.z * 0.35;
      const ay = a.y + py * a.z * 0.35;
      for (let j = i + 1; j < dots.length; j++) {
        const b = dots[j];
        const bx = b.x + px * b.z * 0.35;
        const by = b.y + py * b.z * 0.35;
        const d = Math.hypot(ax - bx, ay - by);
        if (d < 130) {
          ctx.strokeStyle = `rgba(${col.a},${(1 - d / 130) * 0.14})`;
          ctx.beginPath();
          ctx.moveTo(ax, ay);
          ctx.lineTo(bx, by);
          ctx.stroke();
        }
      }
      const glow = 0.35 + Math.sin(t * 2.2 + a.p) * 0.25;
      ctx.beginPath();
      ctx.fillStyle = `rgba(${col.a},${0.25 + glow * 0.4})`;
      ctx.arc(ax, ay, a.r * a.z, 0, Math.PI * 2);
      ctx.fill();
    }

    // soft center ring
    const cx = w * 0.5 + px * 0.15;
    const cy = h * 0.42 + py * 0.1;
    ctx.save();
    ctx.translate(cx, cy);
    ctx.rotate(t * 0.12);
    for (let i = 0; i < 3; i++) {
      ctx.beginPath();
      ctx.strokeStyle = `rgba(${col.a},${0.1 - i * 0.025})`;
      ctx.lineWidth = 1;
      ctx.ellipse(0, 0, 70 + i * 28, 26 + i * 8, 0, 0, Math.PI * 2);
      ctx.stroke();
    }
    ctx.restore();

    raf = requestAnimationFrame(frame);
  }

  window.addEventListener("resize", resize, { passive: true });
  window.addEventListener("pointermove", (e) => {
    mx = e.clientX / Math.max(window.innerWidth, 1);
    my = e.clientY / Math.max(window.innerHeight, 1);
    document.documentElement.style.setProperty("--mx", (mx - 0.5).toFixed(4));
    document.documentElement.style.setProperty("--my", (my - 0.5).toFixed(4));
  }, { passive: true });
  resize();
  raf = requestAnimationFrame(frame);
})();
