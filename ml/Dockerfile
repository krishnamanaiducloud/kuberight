FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY service.py .
USER 65532:65532
EXPOSE 8081
CMD ["uvicorn", "service:app", "--host", "0.0.0.0", "--port", "8081"]
