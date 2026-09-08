// ESLint flat configuration.
//
// Next 16 removed `next lint`, so linting runs ESLint directly. The Next
// config is a flat-config array and is spread in as-is; the additions below
// are the project's own rules.

import next from "eslint-config-next/core-web-vitals";

const config = [
  {
    ignores: [
      ".next/**",
      "node_modules/**",
      "next-env.d.ts",
      // Playwright specs run under a different tsconfig and are typechecked
      // by the Playwright runner, not by the app's build.
      "tests/e2e/**",
    ],
  },
  ...next,
  {
    rules: {
      // Money and prices are decimal strings on purpose. A rule that pushes
      // them through Number() would reintroduce the precision bug the whole
      // stack avoids.
      "no-restricted-globals": [
        "error",
        {
          name: "parseFloat",
          message:
            "Do not parse money or prices into floats. Format decimal strings with lib/format.ts.",
        },
      ],
    },
  },
];

export default config;
