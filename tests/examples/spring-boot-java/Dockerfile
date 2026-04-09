FROM maven:3.9-eclipse-temurin-21 AS build
WORKDIR /src
COPY pom.xml ./
RUN mvn -q -e -B dependency:go-offline
COPY src ./src
RUN mvn -q -e -B package -DskipTests \
    && cp target/spring-boot-app-0.0.1-SNAPSHOT.jar /app.jar

FROM eclipse-temurin:21-jre-alpine
COPY --from=build /app.jar /app.jar
EXPOSE 8080
ENTRYPOINT ["java", "-jar", "/app.jar"]
