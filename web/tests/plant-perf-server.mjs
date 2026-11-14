// Test-only stand-in for the Go server's static half, for running plant-perf.spec.ts on a host
// without Go or a backend: serves a built web/dist with the same SPA fallback as httpapi.spa()
// and the same security headers as httpapi.Boundary(), so the spec's CSP check stays real.
// Every /v1 path is a 404 problem (the spec stubs /v1/** in the browser). Not a product server.
//   npm run build && node tests/plant-perf-server.mjs dist 58176
import { createServer } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { extname, join, normalize } from "node:path";

const [dir = "dist", port = "58176"] = process.argv.slice(2);
const types = { ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".json": "application/json" };
createServer(async (req, res) => {
    res.setHeader("Cache-Control", "no-store");
    res.setHeader("X-Content-Type-Options", "nosniff");
    res.setHeader("Referrer-Policy", "no-referrer");
    res.setHeader("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");
    const path = normalize(decodeURIComponent(new URL(req.url, "http://x").pathname));
    if (path === "/v1" || path.startsWith("/v1/")) {
        res.writeHead(404, { "Content-Type": "application/problem+json" });
        return res.end(JSON.stringify({ type: "about:blank", title: "NOT_FOUND", status: 404, code: "NOT_FOUND" }));
    }
    let file = join(dir, path);
    const isFile = await stat(file).then((s) => s.isFile(), () => false);
    if (!isFile) {
        if (extname(path) !== "") return res.writeHead(404).end();
        file = join(dir, "index.html"); // client-side route
    }
    res.writeHead(200, { "Content-Type": types[extname(file)] ?? "application/octet-stream" });
    res.end(await readFile(file));
}).listen(Number(port), "127.0.0.1");
