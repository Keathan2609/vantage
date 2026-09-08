import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";

import { Shell } from "@/components/Shell";
import { VantageProvider } from "@/lib/store";

import "./globals.css";

export const metadata: Metadata = {
  title: "Vantage — Paper Trading Terminal",
  description:
    "Algorithmic trading, quantitative research and risk control. Paper execution only.",
  // The terminal is not a public page and has nothing to gain from being
  // indexed or previewed.
  robots: { index: false, follow: false },
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  colorScheme: "dark",
  themeColor: "#0c0e10",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <VantageProvider>
          <Shell>{children}</Shell>
        </VantageProvider>
      </body>
    </html>
  );
}
