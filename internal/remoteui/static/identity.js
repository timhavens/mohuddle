const ALGORITHM = { name: "ECDSA", namedCurve: "P-256" };

export function encodeBase64(value) {
  const bytes = value instanceof Uint8Array ? value : new Uint8Array(value);
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

// WebCrypto cannot reliably export the public half of a non-extractable pair
// on every supported mobile browser. Generate an ephemeral extractable pair,
// immediately re-import only its private PKCS#8 bytes as non-extractable, and
// erase the temporary byte view after import.
export async function createDeviceIdentity() {
  const generated = await crypto.subtle.generateKey(ALGORITHM, true, ["sign", "verify"]);
  const [spki, pkcs8] = await Promise.all([
    crypto.subtle.exportKey("spki", generated.publicKey),
    crypto.subtle.exportKey("pkcs8", generated.privateKey),
  ]);
  try {
    const privateKey = await crypto.subtle.importKey(
      "pkcs8",
      pkcs8,
      ALGORITHM,
      false,
      ["sign"],
    );
    return { privateKey, publicKey: encodeBase64(spki) };
  } finally {
    new Uint8Array(pkcs8).fill(0);
  }
}

export async function signChallenge(privateKey, payload) {
  if (!(privateKey instanceof CryptoKey) || privateKey.extractable) {
    throw new Error("The saved device key is unavailable or insecure.");
  }
  const signature = await crypto.subtle.sign(
    { name: "ECDSA", hash: "SHA-256" },
    privateKey,
    new TextEncoder().encode(String(payload)),
  );
  return encodeBase64(signature);
}
