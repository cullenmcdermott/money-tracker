-- One row per Jev request, with the token counts Jev reports, so Settings can show usage and an estimated cost.
CREATE TABLE jev_usage (
  at TIMESTAMPTZ NOT NULL DEFAULT now(),
  questions INT NOT NULL,
  input_tokens INT NOT NULL,
  output_tokens INT NOT NULL
);
