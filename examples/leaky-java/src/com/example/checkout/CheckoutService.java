package com.example.checkout;

import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

/**
 * A checkout service with a classic leak: a session cache that is never evicted.
 * Each "request" stores a CartSnapshot holding a 64 KiB buffer.
 */
public class CheckoutService {
    record CartSnapshot(String sessionId, long createdAt, byte[] payload) {}

    static final class SessionCache {
        private final Map<String, CartSnapshot> bySession = new ConcurrentHashMap<>();
        void put(CartSnapshot s) { bySession.put(s.sessionId(), s); }
        int size() { return bySession.size(); }
    }

    public static void main(String[] args) throws Exception {
        int perSecond = Integer.parseInt(System.getenv().getOrDefault("REQUESTS_PER_SECOND", "100"));
        int warmup = Integer.parseInt(System.getenv().getOrDefault("WARMUP_SECONDS", "20"));
        SessionCache cache = new SessionCache();
        System.out.println("checkout-service started, warming up for " + warmup + "s");
        Thread.sleep(warmup * 1000L);
        long n = 0;
        while (true) {
            for (int i = 0; i < perSecond / 10; i++) {
                byte[] payload = new byte[64 * 1024];
                payload[0] = (byte) n;
                cache.put(new CartSnapshot("session-" + n++, System.currentTimeMillis(), payload));
            }
            if (n % 1000 == 0) {
                System.out.println("handled " + n + " checkouts, " + cache.size() + " sessions cached");
            }
            Thread.sleep(100);
        }
    }
}
