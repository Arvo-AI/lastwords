FROM eclipse-temurin:21-jdk AS build
WORKDIR /src
COPY src ./src
RUN javac -d /out $(find src -name '*.java')

FROM eclipse-temurin:21-jre
COPY --from=build /out /app
# -Xmx above the container limit: the kernel OOM-kills the JVM before Java
# throws OutOfMemoryError, so HeapDumpOnOutOfMemoryError never fires.
ENV JAVA_TOOL_OPTIONS="-Xmx1g -XX:NativeMemoryTracking=summary"
CMD ["java", "-cp", "/app", "com.example.checkout.CheckoutService"]
