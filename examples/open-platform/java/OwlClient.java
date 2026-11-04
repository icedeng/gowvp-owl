// Java 17+，仅标准库；密钥仅从服务端环境读取。
import java.io.ByteArrayOutputStream;
import java.net.URI;
import java.net.http.*;
import java.nio.ByteBuffer;
import java.nio.charset.*;
import java.security.*;
import java.util.*;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

public class OwlClient {
    static String hash(byte[] data) throws Exception {
        return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(data));
    }
    static String encode(String value) {
        StringBuilder out = new StringBuilder();
        for (byte b : value.getBytes(StandardCharsets.UTF_8)) {
            int c = b & 255;
            if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || "-._~".indexOf(c) >= 0) out.append((char)c);
            else out.append(String.format("%%%02X", c));
        }
        return out.toString();
    }
    static String decode(String raw) throws Exception {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        byte[] source = raw.getBytes(StandardCharsets.UTF_8);
        for (int i = 0; i < source.length; i++) {
            int c = source[i] & 255;
            if (c == '%') {
                if (i + 2 >= source.length) throw new IllegalArgumentException("invalid escape");
                int a = Character.digit((char)source[++i], 16), b = Character.digit((char)source[++i], 16);
                if (a < 0 || b < 0) throw new IllegalArgumentException("invalid escape");
                c = (a << 4) | b;
            }
            out.write(c);
        }
        return StandardCharsets.UTF_8.newDecoder().onMalformedInput(CodingErrorAction.REPORT).decode(ByteBuffer.wrap(out.toByteArray())).toString();
    }
    static String canonicalQuery(String raw) throws Exception {
        Set<String> seen = new HashSet<>(); List<String> pairs = new ArrayList<>();
        if (!raw.isEmpty()) for (String pair : raw.split("&", -1)) {
            String[] parts = pair.split("=", 2);
            String k = decode(parts[0]), v = decode(parts.length == 2 ? parts[1] : "");
            if (k.isEmpty() || !seen.add(k)) throw new IllegalArgumentException("duplicate query");
            pairs.add(encode(k) + "=" + encode(v));
        }
        Collections.sort(pairs); return String.join("&", pairs);
    }
    static String canonical(String audience, String method, String path, String query, String ct, String ts, String nonce, String idem, byte[] body) throws Exception {
        if (!path.matches("/[A-Za-z0-9/._~-]+")) throw new IllegalArgumentException("invalid path");
        for (String segment : path.substring(1).split("/", -1)) if (segment.isEmpty() || segment.equals(".") || segment.equals("..")) throw new IllegalArgumentException("invalid path");
        return String.join("\n", "OWL-HMAC-SHA256", audience, method.toUpperCase(Locale.ROOT), path, canonicalQuery(query), ct, ts, nonce, idem, hash(body));
    }
    static String sign(String sk, String text) throws Exception {
        byte[] key = Base64.getUrlDecoder().decode(sk);
        if (key.length != 32) throw new IllegalArgumentException("invalid SK");
        Mac mac = Mac.getInstance("HmacSHA256"); mac.init(new SecretKeySpec(key, "HmacSHA256"));
        return HexFormat.of().formatHex(mac.doFinal(text.getBytes(StandardCharsets.UTF_8)));
    }
    static HttpRequest request(String base, String ak, String sk, String audience, String method, String path, String query, String idem, byte[] body) throws Exception {
        if (!base.startsWith("https://") && !("true".equals(System.getenv("OWL_ALLOW_HTTP")) && base.startsWith("http://"))) throw new IllegalArgumentException("HTTP deployment requires OWL_ALLOW_HTTP=true");
        String ts = Long.toString(System.currentTimeMillis() / 1000);
        byte[] random = new byte[24]; new SecureRandom().nextBytes(random);
        String nonce = Base64.getUrlEncoder().withoutPadding().encodeToString(random);
        String ct = Set.of("GET", "DELETE").contains(method) ? "" : "application/json";
        String signature = sign(sk, canonical(audience, method, path, query, ct, ts, nonce, idem, body));
        HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(base.replaceAll("/$", "") + path + (query.isEmpty() ? "" : "?" + query)))
            .timeout(java.time.Duration.ofSeconds(15)).header("Authorization", "OWL-HMAC-SHA256 Credential=" + ak + ",Signature=" + signature)
            .header("X-Owl-Audience", audience).header("X-Owl-Timestamp", ts).header("X-Owl-Nonce", nonce).header("X-Owl-Content-SHA256", hash(body));
        if (!ct.isEmpty()) b.header("Content-Type", ct);
        if (!idem.isEmpty()) b.header("Idempotency-Key", idem);
        return b.method(method, body.length == 0 ? HttpRequest.BodyPublishers.noBody() : HttpRequest.BodyPublishers.ofByteArray(body)).build();
    }
    // 验签后另须持久化nonce、核对payload ID并按delivery_id去重。
    static boolean verifyCallback(Map<String,String> headers, byte[] body, String callbackSK) {
        try {
            Map<String,String> h = new TreeMap<>(String.CASE_INSENSITIVE_ORDER); h.putAll(headers);
            String ts = h.get("X-Owl-Timestamp"), nonce = h.get("X-Owl-Nonce");
            if (Math.abs(System.currentTimeMillis()/1000 - Long.parseLong(ts)) > 300 || Base64.getUrlDecoder().decode(nonce).length < 16 || !hash(body).equals(h.get("X-Owl-Content-SHA256"))) return false;
            String text = String.join("\n", "OWL-CALLBACK-HMAC-SHA256", ts, nonce, h.get("X-Owl-Event-ID"), h.get("X-Owl-Delivery-ID"), hash(body));
            String expected = "OWL-CALLBACK-HMAC-SHA256 Signature=" + sign(callbackSK, text);
            return MessageDigest.isEqual(expected.getBytes(StandardCharsets.UTF_8), h.get("Authorization").getBytes(StandardCharsets.UTF_8));
        } catch (Exception e) { return false; }
    }
    public static void main(String[] args) throws Exception {
        if (args.length > 0 && args[0].equals("--vector")) {
            byte[] key = new byte[32]; for (int i=0;i<key.length;i++) key[i]=(byte)i;
            String text = canonical("owl-test", "POST", "/openapi/v1/snapshot-tasks", "q=A+B%20C&empty=&label=%E5%91%8A%E8%AD%A6", "application/json", "1700000000", "AAECAwQFBgcICQoLDA0ODw", "capture-001", "{\"channel_id\":\"c1\"}".getBytes(StandardCharsets.UTF_8));
            System.out.println(sign(Base64.getUrlEncoder().withoutPadding().encodeToString(key), text)); return;
        }
        Map<String,String> env = System.getenv();
        HttpRequest request = request(env.get("OWL_API_BASE"), env.get("OWL_AK"), env.get("OWL_SK"), env.get("OWL_AUDIENCE"), "GET", "/openapi/v1/devices", "", "", new byte[0]);
        HttpResponse<String> response = HttpClient.newBuilder().followRedirects(HttpClient.Redirect.NEVER).connectTimeout(java.time.Duration.ofSeconds(10)).build().send(request, HttpResponse.BodyHandlers.ofString());
        System.out.println("HTTP " + response.statusCode()); System.out.println(response.body());
    }
}
