const express = require("express");

const app = express();
const PORT = process.env.PORT || 3000;

app.use(express.json());
app.use(express.urlencoded({ extended: true }));
app.use(express.text());
app.use(express.raw({ type: "*/*" }));

app.all("/webhook", (req, res) => {
  const timestamp = new Date().toISOString();

  console.log("\n========== INCOMING REQUEST ==========");
  console.log(`Timestamp: ${timestamp}`);
  console.log(`Method:    ${req.method}`);
  console.log(`URL:       ${req.originalUrl}`);
  console.log("--- Headers ---");
  console.log(JSON.stringify(req.headers, null, 2));
  console.log("--- Query Params ---");
  console.log(JSON.stringify(req.query, null, 2));
  console.log("--- Body ---");
  console.log(typeof req.body === "object" ? JSON.stringify(req.body, null, 2) : req.body);
  console.log("======================================\n");

  res.json({ status: "ok", received_at: timestamp });
});

app.listen(PORT, () => {
  console.log(`Webhook listener running on http://localhost:${PORT}/webhook`);
});
