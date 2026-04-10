export const metadata = {
  title: "nextjs-compose",
  description: "gocracker test fixture",
};

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
